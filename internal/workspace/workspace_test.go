package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempWorkspace(t *testing.T) *Workspace {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	mustWrite(t, filepath.Join(dir, "internal", "a", "a.go"), "package a\n")
	mustWrite(t, filepath.Join(dir, "node_modules", "junk", "x.js"), "// ignored\n")
	mustWrite(t, filepath.Join(dir, ".git", "config"), "[core]\n")
	return Open(dir)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Path arguments come from an LLM, so traversal is untrusted input, not a
// theoretical concern. The contract is containment: whatever is passed in, the
// resolved path is always inside the workspace.
func TestResolveContainsEveryPath(t *testing.T) {
	ws := tempWorkspace(t)
	root, err := filepath.Abs(ws.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{
		"../../../etc/passwd",
		"internal/../../outside.go",
		"/etc/passwd/../../../../etc/shadow",
		"/main.go",
		"./internal/a/a.go",
		"",
	} {
		got, err := ws.Resolve(in)
		if err != nil {
			t.Fatalf("Resolve(%q) = %v", in, err)
		}
		if got != root && !strings.HasPrefix(got, root+string(os.PathSeparator)) {
			t.Fatalf("Resolve(%q) escaped the workspace: %s", in, got)
		}
	}

	// Clamping means a traversal attempt resolves to something that is simply
	// not there, so the read fails rather than leaking a host file.
	if _, _, err := ws.ReadFile("../../../etc/passwd", 100); err == nil {
		t.Fatal("a traversal read must not succeed")
	}
}

func TestReadFileTruncates(t *testing.T) {
	ws := tempWorkspace(t)
	content, truncated, err := ws.ReadFile("main.go", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(content) != 10 {
		t.Fatalf("content=%q truncated=%v, want a 10-byte truncation", content, truncated)
	}

	if _, _, err := ws.ReadFile("nope.go", 100); err == nil {
		t.Fatal("reading a missing file must error")
	}
	if _, _, err := ws.ReadFile("internal", 100); err == nil {
		t.Fatal("reading a directory must error")
	}
}

func TestListFilesSkipsNoiseDirectories(t *testing.T) {
	ws := tempWorkspace(t)
	files, err := ws.ListFiles(".", 100)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(files, "\n")
	if !strings.Contains(joined, "main.go") || !strings.Contains(joined, filepath.Join("internal", "a", "a.go")) {
		t.Fatalf("expected source files in listing:\n%s", joined)
	}
	for _, noise := range []string{"node_modules", ".git"} {
		if strings.Contains(joined, noise) {
			t.Fatalf("%s must be skipped:\n%s", noise, joined)
		}
	}
}

func TestListFilesRespectsLimit(t *testing.T) {
	ws := tempWorkspace(t)
	files, err := ws.ListFiles(".", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("limit ignored: got %d files", len(files))
	}
}

func TestCloneRefusesWithoutSHA(t *testing.T) {
	if _, err := Clone(t.Context(), t.TempDir(), "o", "r", "", ""); err == nil {
		t.Fatal("cloning without a pinned commit must be refused")
	}
}
