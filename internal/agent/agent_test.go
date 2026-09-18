package agent

import (
	"strings"
	"testing"

	"github.com/Anwesha33/pr-sentinel/internal/diff"
	"github.com/Anwesha33/pr-sentinel/internal/store"
)

const sampleDiff = `diff --git a/internal/wallet/wallet.go b/internal/wallet/wallet.go
--- a/internal/wallet/wallet.go
+++ b/internal/wallet/wallet.go
@@ -10,6 +10,12 @@ func New(db *sql.DB) *Wallet {
 	return &Wallet{db: db}
 }
 
+func (w *Wallet) Debit(ctx context.Context, id, amount int64) error {
+	current, err := w.Balance(ctx, id)
+	if err != nil {
+		return err
+	}
+	return w.write(ctx, id, current-amount)
 }
`

func parseFiles(t *testing.T) []*diff.File {
	t.Helper()
	files, err := diff.Parse(sampleDiff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return files
}

func newAgent() *Agent {
	return &Agent{MinConfidence: 0.5, LineTolerance: 3}
}

func TestValidateAnchorsExactLine(t *testing.T) {
	files := parseFiles(t)
	// Line 13 is "+ current, err := w.Balance(...)", an added line.
	kept, dropped := newAgent().validate([]rawFinding{{
		File: "internal/wallet/wallet.go", Line: 14, Severity: "major",
		Category: "data-integrity", Title: "read-modify-write", Body: "lost update",
		Confidence: 0.9,
	}}, files)

	if len(kept) != 1 || len(dropped) != 0 {
		t.Fatalf("kept=%d dropped=%d, want 1/0", len(kept), len(dropped))
	}
	f := kept[0]
	if !f.Anchored || f.Line != 14 || f.Side != string(diff.SideRight) || f.Position == 0 {
		t.Fatalf("finding not anchored correctly: %+v", f)
	}
}

func TestValidateSnapsNearMissWithinTolerance(t *testing.T) {
	files := parseFiles(t)
	// 19 is past the last added line (18); within tolerance it snaps back.
	kept, _ := newAgent().validate([]rawFinding{{
		File: "internal/wallet/wallet.go", Line: 19, Severity: "major",
		Title: "t", Body: "b", Confidence: 0.8,
	}}, files)
	if len(kept) != 1 || !kept[0].Anchored {
		t.Fatalf("near miss should be snapped and kept: %+v", kept)
	}
	if kept[0].Line == 19 {
		t.Fatalf("line should have been snapped to a line in the diff, got %d", kept[0].Line)
	}
}

func TestValidateKeepsFarMissButUnanchored(t *testing.T) {
	files := parseFiles(t)
	// A finding far outside the diff may still be a true statement about the
	// code, so it is kept for the summary but must never be posted inline —
	// GitHub rejects the whole review if one comment is off the diff.
	kept, _ := newAgent().validate([]rawFinding{{
		File: "internal/wallet/wallet.go", Line: 900, Severity: "major",
		Title: "t", Body: "b", Confidence: 0.8,
	}}, files)
	if len(kept) != 1 {
		t.Fatalf("want the finding kept, got %d", len(kept))
	}
	if kept[0].Anchored {
		t.Fatal("a finding outside the diff must not be marked anchored")
	}
	if !strings.Contains(kept[0].DroppedReason, "file level") {
		t.Fatalf("reason should explain the downgrade, got %q", kept[0].DroppedReason)
	}
}

func TestValidateDropsUnknownFileAndLowConfidence(t *testing.T) {
	files := parseFiles(t)
	kept, dropped := newAgent().validate([]rawFinding{
		{File: "some/other/file.go", Line: 3, Title: "t", Body: "b", Confidence: 0.9},
		{File: "internal/wallet/wallet.go", Line: 14, Title: "t", Body: "b", Confidence: 0.2},
		{File: "internal/wallet/wallet.go", Line: 14, Title: "", Body: "b", Confidence: 0.9},
	}, files)
	if len(kept) != 0 {
		t.Fatalf("nothing should survive: %+v", kept)
	}
	if len(dropped) != 3 {
		t.Fatalf("want 3 dropped, got %d", len(dropped))
	}
	reasons := strings.Join([]string{dropped[0].DroppedReason, dropped[1].DroppedReason, dropped[2].DroppedReason}, "|")
	for _, want := range []string{"not part of this pull request", "confidence", "empty title"} {
		if !strings.Contains(reasons, want) {
			t.Fatalf("missing drop reason %q in %q", want, reasons)
		}
	}
}

func TestValidateMatchesPathLoosely(t *testing.T) {
	files := parseFiles(t)
	// Models often emit the path without a leading directory or with "./".
	kept, _ := newAgent().validate([]rawFinding{{
		File: "./wallet/wallet.go", Line: 14, Title: "t", Body: "b", Confidence: 0.9,
	}}, files)
	if len(kept) != 1 || kept[0].FilePath != "internal/wallet/wallet.go" {
		t.Fatalf("loose path match failed: %+v", kept)
	}
}

func TestValidateDeduplicates(t *testing.T) {
	files := parseFiles(t)
	f := rawFinding{File: "internal/wallet/wallet.go", Line: 14, Title: "same", Body: "b", Confidence: 0.9}
	kept, dropped := newAgent().validate([]rawFinding{f, f}, files)
	if len(kept) != 1 || len(dropped) != 1 {
		t.Fatalf("kept=%d dropped=%d, want 1/1", len(kept), len(dropped))
	}
	if !strings.Contains(dropped[0].DroppedReason, "duplicate") {
		t.Fatalf("reason = %q", dropped[0].DroppedReason)
	}
}

func TestValidateSortsCriticalFirst(t *testing.T) {
	files := parseFiles(t)
	kept, _ := newAgent().validate([]rawFinding{
		{File: "internal/wallet/wallet.go", Line: 14, Severity: "minor", Title: "m", Body: "b", Confidence: 0.9},
		{File: "internal/wallet/wallet.go", Line: 15, Severity: "critical", Title: "c", Body: "b", Confidence: 0.9},
	}, files)
	if len(kept) != 2 || kept[0].Severity != "critical" {
		t.Fatalf("critical findings must sort first: %+v", kept)
	}
}

func TestNormalizeSeverity(t *testing.T) {
	for in, want := range map[string]string{
		"CRITICAL": "critical", "blocker": "critical", "high": "critical",
		"major": "major", "medium": "major",
		"minor": "minor", "nit": "minor", "low": "minor",
		"":      "major", // an unlabelled finding is not silently downgraded
		"weird": "major",
	} {
		if got := normalizeSeverity(in); got != want {
			t.Fatalf("normalizeSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseExtractionToleratesFencedJSON(t *testing.T) {
	out, err := parseExtraction("Here you go:\n```json\n{\"summary\":\"s\",\"verdict\":\"comment\",\"findings\":[]}\n```")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.Summary != "s" || out.Verdict != "comment" {
		t.Fatalf("got %+v", out)
	}
}

func TestRenderReviewBodyIncludesUnanchoredFindings(t *testing.T) {
	res := &Result{
		Summary: "Looks mostly fine.",
		Findings: []store.Finding{
			{FilePath: "a.go", Line: 3, Severity: "major", Title: "inline one", Body: "b", Anchored: true},
			{FilePath: "b.go", Line: 9, Severity: "critical", Title: "could not anchor", Body: "body text", Anchored: false},
		},
	}
	body := RenderReviewBody(res, "test-model", true)
	if !strings.Contains(body, "could not anchor") || !strings.Contains(body, "body text") {
		t.Fatalf("unanchored findings must appear in the summary:\n%s", body)
	}
	if strings.Contains(body, "inline one") {
		t.Fatalf("anchored findings belong inline, not duplicated in the body:\n%s", body)
	}
	if !strings.Contains(body, "Dry run") {
		t.Fatalf("a dry run must say so:\n%s", body)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"**/*.go", "internal/store/store.go", true},
		{"**/*.go", "README.md", false},
		{"*.go", "main.go", true},
		{"*.go", "internal/store/store.go", true}, // falls back to basename
		{"internal/*", "internal/store", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.path); got != c.want {
			t.Fatalf("globMatch(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}
