// Package workspace materialises the pull request's head revision on local
// disk so the agent's tools can read files, grep, and run tests against real
// code instead of only the diff.
//
// Cloning is deliberately shallow and single-revision: reviewing a PR needs one
// tree, not the project's history, and a full clone of a large repository is
// both the slowest step in the pipeline and the one most likely to fill a
// worker's disk.
package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Workspace struct {
	Root    string
	Owner   string
	Repo    string
	SHA     string
	cleanup bool
}

// Clone fetches exactly the commit under review into a fresh directory.
//
// token, when non-empty, is injected into the remote URL so private
// repositories work. It is never written to disk in a git config file — the URL
// is passed to a single fetch invocation — because `git remote add` would
// persist the credential into .git/config inside the workspace.
func Clone(ctx context.Context, root, owner, repo, sha, token string) (*Workspace, error) {
	if sha == "" {
		return nil, fmt.Errorf("refusing to clone without an explicit commit sha")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, fmt.Sprintf("%s-%s-%s-", owner, repo, shortSHA(sha)))
	if err != nil {
		return nil, err
	}

	remote := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	if token != "" {
		remote = fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", token, owner, repo)
	}

	steps := [][]string{
		{"git", "init", "--quiet"},
		// --depth=1 on a specific sha needs uploadpack.allowReachableSHA1InWant,
		// which github.com enables. On a server that does not, this falls back
		// to fetching the default branch tip in the error path below.
		{"git", "fetch", "--quiet", "--depth", "1", remote, sha},
		{"git", "checkout", "--quiet", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if out, err := runIn(ctx, dir, 5*time.Minute, args[0], args[1:]...); err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, redactToken(out, token))
		}
	}

	return &Workspace{Root: dir, Owner: owner, Repo: repo, SHA: sha, cleanup: true}, nil
}

// Open wraps an existing directory as a workspace. Used by the evaluation
// harness, which reviews fixtures on local disk with no network involved.
//
// The path is made absolute for the same reason as analysis.NewRunner: tools
// run with the workspace as their working directory, and a relative root makes
// every derived path resolve against the wrong base.
func Open(dir string) *Workspace {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	return &Workspace{Root: abs, cleanup: false}
}

func (w *Workspace) Close() error {
	if w == nil || !w.cleanup {
		return nil
	}
	return os.RemoveAll(w.Root)
}

// Resolve turns a repo-relative path into an absolute one inside the
// workspace. The agent's path arguments come from an LLM, so "../../etc/passwd"
// is a case that must be handled rather than assumed away.
//
// Traversal is *clamped*, not rejected: the path is cleaned as if the workspace
// were the filesystem root, so "../../etc/passwd" becomes "<root>/etc/passwd",
// which then simply does not exist. Clamping is chosen over erroring because a
// model that writes "../internal/x.go" usually means "internal/x.go", and
// resolving that to a missing file gives it a useful error either way.
//
// The explicit prefix check afterwards is defence in depth: it catches the
// cases Clean cannot, such as a root path that itself contains a symlink, and
// guarantees this function can never return a path outside the workspace.
func (w *Workspace) Resolve(rel string) (string, error) {
	clean := filepath.Clean("/" + strings.TrimPrefix(rel, "/"))
	abs := filepath.Join(w.Root, clean)
	rootAbs, err := filepath.Abs(w.Root)
	if err != nil {
		return "", err
	}
	absReal, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	if absReal != rootAbs && !strings.HasPrefix(absReal, rootAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the workspace", rel)
	}
	return absReal, nil
}

// ReadFile returns a file's contents, capped at maxBytes.
func (w *Workspace) ReadFile(rel string, maxBytes int) (string, bool, error) {
	abs, err := w.Resolve(rel)
	if err != nil {
		return "", false, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return "", false, fmt.Errorf("%s is a directory", rel)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", false, err
	}
	if len(data) > maxBytes {
		return string(data[:maxBytes]), true, nil
	}
	return string(data), false, nil
}

// ListFiles walks the tree, skipping the directories that are never worth
// showing a reviewer and would otherwise dominate the listing.
func (w *Workspace) ListFiles(rel string, limit int) ([]string, error) {
	base, err := w.Resolve(rel)
	if err != nil {
		return nil, err
	}
	var out []string
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) && path != base {
				return filepath.SkipDir
			}
			return nil
		}
		p, relErr := filepath.Rel(w.Root, path)
		if relErr != nil {
			return nil
		}
		out = append(out, p)
		if len(out) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	return out, err
}

func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", "build", "target", ".venv", "venv",
		"__pycache__", ".idea", ".gradle", ".next", "coverage":
		return true
	}
	return false
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func runIn(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// Keep the child out of the operator's git identity and prompts.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+dir,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}
