package diff

import "testing"

// A two-hunk diff. Positions are counted from the first "@@" header: the line
// below it is 1, and the second "@@" header itself consumes a slot.
const twoHunk = `diff --git a/pkg/cart/total.go b/pkg/cart/total.go
index 1111111..2222222 100644
--- a/pkg/cart/total.go
+++ b/pkg/cart/total.go
@@ -10,6 +10,7 @@ func Total(items []Item) int {
 	sum := 0
 	for _, it := range items {
-		sum += it.Price
+		sum += it.Price * it.Qty
+		_ = it
 	}
 	return sum
 }
@@ -40,3 +41,4 @@ func Discount(total int) int {
 	if total > 100 {
 		return total / 10
 	}
+	return 0
 }
`

func TestParsePositionsAndSides(t *testing.T) {
	files, err := Parse(twoHunk)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Path() != "pkg/cart/total.go" {
		t.Fatalf("path = %q", f.Path())
	}
	if len(f.Hunks) != 2 {
		t.Fatalf("want 2 hunks, got %d", len(f.Hunks))
	}

	// Hunk 1 starts at new line 10. Body lines in order:
	//  pos 1: " sum := 0"                  -> new 10 (context)
	//  pos 2: " for _, it := range items"  -> new 11 (context)
	//  pos 3: "-sum += it.Price"           -> old 12 (LEFT)
	//  pos 4: "+sum += it.Price * it.Qty"  -> new 12 (added)
	//  pos 5: "+_ = it"                    -> new 13 (added)
	//  pos 6: " }"                         -> new 14
	a, ok := f.Anchor(SideRight, 12)
	if !ok || !a.Added || a.Position != 4 {
		t.Fatalf("added line 12: anchor=%+v ok=%v, want position 4 added", a, ok)
	}
	if a2, ok := f.Anchor(SideRight, 13); !ok || a2.Position != 5 {
		t.Fatalf("added line 13: anchor=%+v ok=%v, want position 5", a2, ok)
	}

	// The deleted line is only addressable on the LEFT side, at the OLD number.
	del, ok := f.Anchor(SideLeft, 12)
	if !ok || del.Position != 3 || del.Added {
		t.Fatalf("deleted line: anchor=%+v ok=%v, want position 3 on LEFT", del, ok)
	}
	if _, ok := f.Anchor(SideLeft, 999); ok {
		t.Fatalf("line outside the diff must not resolve")
	}

	// Hunk 2: positions continue across the header. Hunk 1 body is 8 lines
	// (pos 1..8), the second "@@" is pos 9, so its first body line is pos 10.
	// Its added line "return 0" is the 4th body line of hunk 2 -> pos 13.
	if got, ok := f.Anchor(SideRight, 44); !ok || got.Position != 13 {
		t.Fatalf("second-hunk added line: anchor=%+v ok=%v, want position 13", got, ok)
	}
}

func TestAddedLinesOnly(t *testing.T) {
	files, _ := Parse(twoHunk)
	added := files[0].AddedLines()
	want := []int{12, 13, 44}
	if len(added) != len(want) {
		t.Fatalf("added lines = %+v, want %v", added, want)
	}
	for i, a := range added {
		if a.Line != want[i] || !a.Added || a.Side != SideRight {
			t.Fatalf("added[%d] = %+v, want line %d RIGHT", i, a, want[i])
		}
	}
}

func TestNearestAddedLineSnapsSmallMisses(t *testing.T) {
	files, _ := Parse(twoHunk)
	f := files[0]
	// The model claims line 14; the nearest line the PR actually touched is 13.
	got, ok := f.NearestAddedLine(14, 3)
	if !ok || got.Line != 13 {
		t.Fatalf("snap(14) = %+v ok=%v, want line 13", got, ok)
	}
	// Way outside tolerance: refuse to snap, caller falls back to file-level.
	if _, ok := f.NearestAddedLine(500, 3); ok {
		t.Fatalf("snap(500) must fail rather than inventing an anchor")
	}
}

func TestParseNewFileAndBinary(t *testing.T) {
	const raw = `diff --git a/new.txt b/new.txt
new file mode 100644
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+hello
+world
diff --git a/logo.png b/logo.png
index 3333333..4444444 100644
Binary files a/logo.png and b/logo.png differ
`
	files, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	if files[0].Path() != "new.txt" || len(files[0].AddedLines()) != 2 {
		t.Fatalf("new file parsed wrong: %+v", files[0])
	}
	if a, ok := files[0].Anchor(SideRight, 1); !ok || a.Position != 1 {
		t.Fatalf("first added line of a new file must be position 1, got %+v", a)
	}
	if !files[1].IsBinary {
		t.Fatalf("binary file not flagged")
	}
	if len(files[1].AddedLines()) != 0 {
		t.Fatalf("binary file must expose no commentable lines")
	}
}

func TestSummarize(t *testing.T) {
	files, _ := Parse(twoHunk)
	s := Summarize(files)
	if s.Files != 1 || s.Additions != 3 || s.Deletions != 1 {
		t.Fatalf("stats = %+v, want 1 file / +3 / -1", s)
	}
}

func TestRenderCarriesRealLineNumbers(t *testing.T) {
	files, _ := Parse(twoHunk)
	out := Render(files, 1<<20)
	if !contains(out, "    12 + \t\tsum += it.Price * it.Qty") {
		t.Fatalf("render must annotate added lines with head line numbers:\n%s", out)
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
