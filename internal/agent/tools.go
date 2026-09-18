// Tool definitions and their implementations.
//
// Design rules this file follows, learned from watching the agent misbehave
// against real pull requests:
//
//  1. Every tool returns a *bounded* result. An unbounded read_file on a 40k
//     line generated file eats the context window and the model then reviews
//     nothing but that file.
//  2. Every failure is returned to the model as a normal result with an
//     `error` field, never as a Go error that aborts the loop. The model
//     recovers from "file not found: did you mean X" perfectly well; it cannot
//     recover from the loop being torn down.
//  3. Tool arguments are validated against the workspace before use. They are
//     model output, which means they are untrusted input.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Anwesha33/pr-sentinel/internal/analysis"
	"github.com/Anwesha33/pr-sentinel/internal/cache"
	"github.com/Anwesha33/pr-sentinel/internal/llm"
	"github.com/Anwesha33/pr-sentinel/internal/workspace"
)

const (
	toolReadFile    = "read_file"
	toolSearchCode  = "search_code"
	toolListFiles   = "list_files"
	toolRunTests    = "run_tests"
	toolStaticCheck = "run_static_analysis"
)

// ToolDeclarations is the schema handed to the model.
func ToolDeclarations() []llm.FunctionDeclaration {
	return []llm.FunctionDeclaration{
		{
			Name: toolReadFile,
			Description: "Read a file from the pull request's head revision. Use this to see " +
				"code the diff references but does not show — the function being called, the " +
				"struct being modified, the test that covers the changed path. Returns at most " +
				"400 lines; pass start_line/end_line to page through a larger file.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Repository-relative path, e.g. internal/store/store.go",
					},
					"start_line": map[string]any{
						"type":        "integer",
						"description": "1-based first line to return. Omit to start at the top.",
					},
					"end_line": map[string]any{
						"type":        "integer",
						"description": "1-based last line to return.",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name: toolSearchCode,
			Description: "Search the repository for a regular expression and return matching " +
				"lines with their file and line number. Use it to find callers of a changed " +
				"function, other places that repeat a bug, or whether a helper already exists.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "RE2 regular expression, e.g. func \\(s \\*Store\\) Claim",
					},
					"path_glob": map[string]any{
						"type":        "string",
						"description": "Optional glob to restrict the search, e.g. **/*.go",
					},
					"max_results": map[string]any{
						"type":        "integer",
						"description": "Maximum matches to return (default 40, hard cap 100).",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        toolListFiles,
			Description: "List files under a directory of the repository, to orient yourself before reading.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"dir": map[string]any{
						"type":        "string",
						"description": "Repository-relative directory. Use \".\" for the root.",
					},
				},
				"required": []string{"dir"},
			},
		},
		{
			Name: toolRunTests,
			Description: "Run the project's own test suite against the pull request's head " +
				"revision and return the result. Call this at most once, and only when you " +
				"suspect the change breaks behaviour that tests would catch — it is the " +
				"slowest tool available.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name: toolStaticCheck,
			Description: "Run the language's static analysis (vet/lint/compile) and return the " +
				"diagnostics. Results are already included in your first message; call this " +
				"again only if you need the full output.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

// Toolbox executes tool calls against one workspace.
type Toolbox struct {
	WS           *workspace.Workspace
	Runner       *analysis.Runner
	Cache        *cache.Cache
	MaxFileBytes int

	// testsRun enforces the "at most once" rule in code rather than trusting
	// the prompt. The model will otherwise re-run a 3-minute suite whenever it
	// feels uncertain.
	testsRun   bool
	staticRuns int
}

// ToolOutcome is what the loop records for observability.
type ToolOutcome struct {
	Payload  map[string]any
	Summary  string
	Size     int
	CacheHit bool
	Err      string
}

func (t *Toolbox) Execute(ctx context.Context, call llm.FunctionCall) ToolOutcome {
	switch call.Name {
	case toolReadFile:
		return t.readFile(ctx, call.Args)
	case toolSearchCode:
		return t.searchCode(ctx, call.Args)
	case toolListFiles:
		return t.listFiles(call.Args)
	case toolRunTests:
		return t.runTests(ctx)
	case toolStaticCheck:
		return t.runStatic(ctx)
	default:
		return errOutcome(fmt.Sprintf("unknown tool %q; available tools are: %s",
			call.Name, strings.Join([]string{toolReadFile, toolSearchCode, toolListFiles, toolRunTests, toolStaticCheck}, ", ")))
	}
}

const maxReadLines = 400

func (t *Toolbox) readFile(ctx context.Context, args map[string]any) ToolOutcome {
	path := argString(args, "path")
	if path == "" {
		return errOutcome("read_file requires a 'path' argument")
	}
	start := argInt(args, "start_line", 1)
	end := argInt(args, "end_line", 0)

	key := cache.Key("file", t.WS.Root, path)
	var content string
	hit := false
	if t.Cache != nil {
		if v, err := t.Cache.Get(ctx, key); err == nil {
			content, hit = v, true
		}
	}
	if !hit {
		c, truncated, err := t.WS.ReadFile(path, t.MaxFileBytes)
		if err != nil {
			// A wrong path is the single most common tool error. Spending one
			// directory listing to suggest the right file is far cheaper than
			// letting the model guess again.
			return errOutcome(fmt.Sprintf("cannot read %q: %v. Nearby files: %s",
				path, err, strings.Join(t.suggest(path), ", ")))
		}
		content = c
		if truncated {
			content += "\n...[file truncated at MAX_FILE_READ_BYTES]..."
		}
		if t.Cache != nil {
			_ = t.Cache.Set(ctx, key, content, 30*time.Minute)
		}
	}

	lines := strings.Split(content, "\n")
	if start < 1 {
		start = 1
	}
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) {
		return errOutcome(fmt.Sprintf("%s has %d lines; start_line %d is past the end", path, len(lines), start))
	}
	if end-start+1 > maxReadLines {
		end = start + maxReadLines - 1
	}

	var b strings.Builder
	for i := start; i <= end && i <= len(lines); i++ {
		fmt.Fprintf(&b, "%6d  %s\n", i, lines[i-1])
	}
	body := b.String()
	return ToolOutcome{
		Payload: map[string]any{
			"path": path, "start_line": start, "end_line": end,
			"total_lines": len(lines), "content": body,
		},
		Summary:  fmt.Sprintf("%s lines %d-%d of %d", path, start, end, len(lines)),
		Size:     len(body),
		CacheHit: hit,
	}
}

func (t *Toolbox) suggest(path string) []string {
	dir := filepath.Dir(path)
	files, err := t.WS.ListFiles(dir, 12)
	if err != nil || len(files) == 0 {
		files, err = t.WS.ListFiles(".", 12)
		if err != nil {
			return []string{"(none)"}
		}
	}
	return files
}

func (t *Toolbox) searchCode(ctx context.Context, args map[string]any) ToolOutcome {
	query := argString(args, "query")
	if query == "" {
		return errOutcome("search_code requires a 'query' argument")
	}
	limit := argInt(args, "max_results", 40)
	if limit <= 0 || limit > 100 {
		limit = 40
	}
	glob := argString(args, "path_glob")

	re, err := regexp.Compile(query)
	if err != nil {
		// Models write PCRE-isms like (?<name>...) that RE2 rejects. Falling
		// back to a literal search keeps the step useful instead of wasting it.
		lit, litErr := regexp.Compile(regexp.QuoteMeta(query))
		if litErr != nil {
			return errOutcome(fmt.Sprintf("invalid regex %q: %v", query, err))
		}
		re = lit
	}

	key := cache.Key("search", t.WS.Root, query, glob, fmt.Sprint(limit))
	if t.Cache != nil {
		if v, cErr := t.Cache.Get(ctx, key); cErr == nil {
			var payload map[string]any
			if json.Unmarshal([]byte(v), &payload) == nil {
				return ToolOutcome{Payload: payload, Summary: "search (cached): " + query, Size: len(v), CacheHit: true}
			}
		}
	}

	files, err := t.WS.ListFiles(".", 20000)
	if err != nil {
		return errOutcome(fmt.Sprintf("cannot walk repository: %v", err))
	}
	sort.Strings(files)

	type match struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	matches := []match{}
	for _, rel := range files {
		if glob != "" && !globMatch(glob, rel) {
			continue
		}
		if isProbablyBinaryPath(rel) {
			continue
		}
		abs, err := t.WS.Resolve(rel)
		if err != nil {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil || info.Size() > 2<<20 {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil || isBinary(data) {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				matches = append(matches, match{Path: rel, Line: i + 1, Text: trimLine(line)})
				if len(matches) >= limit {
					break
				}
			}
		}
		if len(matches) >= limit {
			break
		}
	}

	payload := map[string]any{"query": query, "matches": matches, "truncated": len(matches) >= limit}
	if t.Cache != nil {
		if raw, err := json.Marshal(payload); err == nil {
			_ = t.Cache.Set(ctx, key, string(raw), 15*time.Minute)
		}
	}
	return ToolOutcome{
		Payload: payload,
		Summary: fmt.Sprintf("%d matches for %q", len(matches), query),
		Size:    len(matches),
	}
}

func (t *Toolbox) listFiles(args map[string]any) ToolOutcome {
	dir := argString(args, "dir")
	if dir == "" {
		dir = "."
	}
	files, err := t.WS.ListFiles(dir, 300)
	if err != nil {
		return errOutcome(fmt.Sprintf("cannot list %q: %v", dir, err))
	}
	return ToolOutcome{
		Payload: map[string]any{"dir": dir, "files": files, "count": len(files)},
		Summary: fmt.Sprintf("%d files under %s", len(files), dir),
		Size:    len(files),
	}
}

func (t *Toolbox) runTests(ctx context.Context) ToolOutcome {
	if t.testsRun {
		return ToolOutcome{
			Payload: map[string]any{"error": "run_tests has already been called for this review; " +
				"the result is unchanged. Use the earlier output."},
			Summary: "run_tests refused (already run)",
		}
	}
	t.testsRun = true
	res := t.Runner.Tests(ctx)
	return ToolOutcome{
		Payload: map[string]any{
			"command": res.Command, "passed": res.Passed, "skipped": res.Skipped,
			"skip_note": res.SkipNote, "timed_out": res.TimedOut,
			"exit_code": res.ExitCode, "output": res.Output,
			"duration_seconds": res.Duration.Seconds(),
		},
		Summary: fmt.Sprintf("tests: passed=%v skipped=%v exit=%d in %s",
			res.Passed, res.Skipped, res.ExitCode, res.Duration.Round(time.Second)),
		Size: len(res.Output),
	}
}

func (t *Toolbox) runStatic(ctx context.Context) ToolOutcome {
	if t.staticRuns >= 2 {
		return ToolOutcome{
			Payload: map[string]any{"error": "run_static_analysis has already been called twice; reuse the earlier output."},
			Summary: "run_static_analysis refused (call limit)",
		}
	}
	t.staticRuns++
	results := t.Runner.StaticAnalysis(ctx)
	summary := analysis.Summarize(results)
	return ToolOutcome{
		Payload: map[string]any{"report": summary},
		Summary: fmt.Sprintf("static analysis: %d checks", len(results)),
		Size:    len(summary),
	}
}

func errOutcome(msg string) ToolOutcome {
	return ToolOutcome{
		Payload: map[string]any{"error": msg},
		Summary: msg,
		Err:     msg,
		Size:    len(msg),
	}
}

func argString(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

func argInt(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		var parsed int
		if _, err := fmt.Sscanf(n, "%d", &parsed); err == nil {
			return parsed
		}
	}
	return def
}

// globMatch supports the "**/" prefix that models reliably produce, which
// filepath.Match does not understand.
func globMatch(pattern, path string) bool {
	if strings.HasPrefix(pattern, "**/") {
		suffix := strings.TrimPrefix(pattern, "**/")
		if ok, _ := filepath.Match(suffix, filepath.Base(path)); ok {
			return true
		}
		return strings.HasSuffix(path, strings.TrimPrefix(suffix, "*"))
	}
	if ok, _ := filepath.Match(pattern, path); ok {
		return true
	}
	ok, _ := filepath.Match(pattern, filepath.Base(path))
	return ok
}

func isProbablyBinaryPath(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".pdf", ".zip", ".gz", ".tar", ".jar",
		".class", ".so", ".dylib", ".dll", ".exe", ".woff", ".woff2", ".ttf", ".ico", ".mp4":
		return true
	}
	return false
}

func isBinary(data []byte) bool {
	n := len(data)
	if n > 1024 {
		n = 1024
	}
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

func trimLine(s string) string {
	s = strings.TrimRight(s, "\r")
	if len(s) > 240 {
		return s[:240] + "…"
	}
	return s
}
