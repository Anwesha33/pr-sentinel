// Package analysis runs the deterministic half of a review: toolchain
// detection, static analysis, and the project's own test suite.
//
// The LLM is good at "this looks wrong"; a compiler is definitive about it.
// Running both and feeding the compiler's output back into the agent is what
// stops the review from being a pile of plausible-sounding guesses.
//
// Execution safety: commands are chosen from a fixed allowlist, never
// assembled from model output. The agent may ask to "run the tests"; it cannot
// ask to run an arbitrary shell string. Each command gets a timeout, a scratch
// HOME inside the workspace, and a truncated output buffer.
package analysis

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Toolchain is the build system detected in a workspace.
type Toolchain string

const (
	ToolchainGo      Toolchain = "go"
	ToolchainNode    Toolchain = "node"
	ToolchainPython  Toolchain = "python"
	ToolchainMaven   Toolchain = "maven"
	ToolchainGradle  Toolchain = "gradle"
	ToolchainUnknown Toolchain = "unknown"
)

// Detect inspects marker files. Order matters: a Go service with a small
// frontend has both go.mod and package.json, and the Go suite is the one that
// gates the merge.
func Detect(root string) Toolchain {
	exists := func(p string) bool {
		_, err := os.Stat(filepath.Join(root, p))
		return err == nil
	}
	switch {
	case exists("go.mod"):
		return ToolchainGo
	case exists("pom.xml"):
		return ToolchainMaven
	case exists("build.gradle") || exists("build.gradle.kts"):
		return ToolchainGradle
	case exists("pyproject.toml") || exists("setup.py") || exists("requirements.txt"):
		return ToolchainPython
	case exists("package.json"):
		return ToolchainNode
	default:
		return ToolchainUnknown
	}
}

// Result is the outcome of one command.
type Result struct {
	Command   string        `json:"command"`
	Skipped   bool          `json:"skipped"`
	SkipNote  string        `json:"skip_note,omitempty"`
	ExitCode  int           `json:"exit_code"`
	Passed    bool          `json:"passed"`
	TimedOut  bool          `json:"timed_out"`
	Output    string        `json:"output"`
	Truncated bool          `json:"truncated"`
	Duration  time.Duration `json:"duration"`
}

// Runner executes allowlisted commands inside a workspace.
type Runner struct {
	Root      string
	Timeout   time.Duration
	MaxOutput int
	// AllowNetwork is false by default. Test suites that reach the network are
	// both slow and untrustworthy in a review bot; when the flag is off the
	// package managers are told to work offline where they support it.
	AllowNetwork bool
}

// NewRunner resolves root to an absolute path before storing it.
//
// This is not tidiness. Every command runs with cmd.Dir = Root *and* with
// HOME=Root so the toolchain writes its caches inside the workspace. If Root is
// relative, the child resolves HOME against its own new working directory, so
// HOME lands at <root>/<root> — and the Go toolchain then builds a fresh cache
// tree inside the workspace on every single invocation. Left relative, this
// silently nests one directory deeper per run and makes every command slow.
func NewRunner(root string) *Runner {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	return &Runner{Root: abs, Timeout: 4 * time.Minute, MaxOutput: 24000}
}

// allowlist maps a logical action to the exact argv permitted for it. Nothing
// outside this table can ever be executed.
var allowlist = map[Toolchain]struct {
	Static [][]string
	Test   [][]string
	Build  [][]string
}{
	ToolchainGo: {
		Static: [][]string{{"gofmt", "-l", "."}, {"go", "vet", "./..."}},
		Test:   [][]string{{"go", "test", "./...", "-count=1"}},
		Build:  [][]string{{"go", "build", "./..."}},
	},
	ToolchainNode: {
		Static: [][]string{{"npx", "--no-install", "eslint", "."}},
		Test:   [][]string{{"npm", "test", "--silent"}},
		Build:  [][]string{{"npm", "run", "build", "--if-present"}},
	},
	ToolchainPython: {
		Static: [][]string{{"python3", "-m", "ruff", "check", "."}, {"python3", "-m", "compileall", "-q", "."}},
		Test:   [][]string{{"python3", "-m", "pytest", "-q"}},
	},
	ToolchainMaven: {
		Static: [][]string{{"mvn", "-q", "-o", "compile"}},
		Test:   [][]string{{"mvn", "-q", "-o", "test"}},
	},
	ToolchainGradle: {
		Static: [][]string{{"./gradlew", "--offline", "compileJava"}},
		Test:   [][]string{{"./gradlew", "--offline", "test"}},
	},
}

// StaticAnalysis runs every static check for the detected toolchain. A missing
// linter is reported as skipped rather than failed: "eslint is not installed"
// is not a finding about the pull request.
func (r *Runner) StaticAnalysis(ctx context.Context) []Result {
	tc := Detect(r.Root)
	cmds := allowlist[tc].Static
	if len(cmds) == 0 {
		return []Result{{Command: "static-analysis", Skipped: true,
			SkipNote: fmt.Sprintf("no static analysis configured for toolchain %q", tc)}}
	}
	var out []Result
	for _, c := range cmds {
		out = append(out, r.run(ctx, c))
	}
	return out
}

// Tests runs the project's suite.
func (r *Runner) Tests(ctx context.Context) Result {
	tc := Detect(r.Root)
	cmds := allowlist[tc].Test
	if len(cmds) == 0 {
		return Result{Command: "tests", Skipped: true,
			SkipNote: fmt.Sprintf("no test command known for toolchain %q", tc)}
	}
	return r.run(ctx, cmds[0])
}

// Build compiles without running tests. Cheaper than the suite and catches the
// single most embarrassing class of AI review finding: commenting on code that
// does not compile in the first place.
func (r *Runner) Build(ctx context.Context) Result {
	tc := Detect(r.Root)
	cmds := allowlist[tc].Build
	if len(cmds) == 0 {
		return Result{Command: "build", Skipped: true, SkipNote: "no build command for this toolchain"}
	}
	return r.run(ctx, cmds[0])
}

func (r *Runner) run(ctx context.Context, argv []string) Result {
	res := Result{Command: strings.Join(argv, " ")}

	if _, err := exec.LookPath(argv[0]); err != nil && !strings.HasPrefix(argv[0], "./") {
		res.Skipped = true
		res.SkipNote = fmt.Sprintf("%s is not installed on this worker", argv[0])
		return res
	}

	timeout := r.Timeout
	if timeout == 0 {
		timeout = 3 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = r.Root
	cmd.Env = r.env()
	raw, err := cmd.CombinedOutput()
	res.Duration = time.Since(start)

	max := r.MaxOutput
	if max == 0 {
		max = 20000
	}
	out := string(raw)
	if len(out) > max {
		// Keep the head and the tail: compilers put the first error at the top,
		// test runners put the summary at the bottom.
		half := max / 2
		out = out[:half] + "\n...[truncated]...\n" + out[len(out)-half:]
		res.Truncated = true
	}
	res.Output = out

	if runCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.Passed = false
		res.ExitCode = -1
		return res
	}
	if err != nil {
		var ee *exec.ExitError
		if asExitError(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			res.Output += "\n" + err.Error()
		}
		res.Passed = false
		return res
	}
	// gofmt is the awkward one: it exits 0 and prints the offending file names,
	// so an empty stdout is the actual pass condition.
	if argv[0] == "gofmt" {
		res.Passed = strings.TrimSpace(res.Output) == ""
		return res
	}
	res.Passed = true
	return res
}

func (r *Runner) env() []string {
	env := append(os.Environ(),
		"HOME="+r.Root,
		"GOFLAGS=-mod=mod",
		"GOCACHE="+filepath.Join(r.Root, ".cache", "go-build"),
		"GOMODCACHE="+filepath.Join(r.Root, ".cache", "go-mod"),
		"CI=true",
		"NO_COLOR=1",
		// HOME points inside the workspace, so anything the toolchain writes to
		// the home directory lands in the tree under review. Go's telemetry
		// counters are the main offender; turn them off rather than leaving
		// stray files in a repository we do not own.
		"GOTELEMETRY=off",
		"GOTELEMETRYDIR="+filepath.Join(r.Root, ".cache", "telemetry"),
		// Never let a repository's go.mod trigger a toolchain download.
		"GOTOOLCHAIN=local",
	)
	if !r.AllowNetwork {
		env = append(env, "GOPROXY=off", "PIP_NO_INDEX=1", "npm_config_offline=true")
	}
	return env
}

// Summarize renders results for the LLM prompt. Passing checks are collapsed to
// one line — the model only needs detail where something went wrong.
func Summarize(results []Result) string {
	var b strings.Builder
	for _, r := range results {
		switch {
		case r.Skipped:
			fmt.Fprintf(&b, "- SKIPPED  %s (%s)\n", r.Command, r.SkipNote)
		case r.TimedOut:
			fmt.Fprintf(&b, "- TIMEOUT  %s after %s\n", r.Command, r.Duration.Round(time.Second))
		case r.Passed:
			fmt.Fprintf(&b, "- PASS     %s (%s)\n", r.Command, r.Duration.Round(time.Millisecond))
		default:
			fmt.Fprintf(&b, "- FAIL     %s (exit %d)\n```\n%s\n```\n", r.Command, r.ExitCode, strings.TrimSpace(r.Output))
		}
	}
	if b.Len() == 0 {
		return "(no checks run)"
	}
	return b.String()
}
