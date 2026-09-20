// Package diff parses unified diffs produced by the GitHub "compare" and
// "pull request .diff" endpoints.
//
// The important output is not the text — it is the mapping from
// (file, line-number-in-the-head-revision) to a location GitHub will accept on
// a review comment. GitHub exposes two ways to anchor an inline comment:
//
//   - the modern `line` + `side` pair, which uses real file line numbers, and
//   - the legacy `position`, which is an offset counted in *diff lines* from
//     the first "@@" hunk header of that file.
//
// We compute both. `line`/`side` is what we send; `position` is kept because
// some GitHub Enterprise versions and a few third-party mirrors still only
// accept the legacy form, and because it is the only way to comment on a line
// that is unchanged context inside a hunk on older API versions.
//
// Anchoring rules that cost real debugging time and are encoded here:
//   - A comment may only be attached to a line that appears in the diff.
//     Anything else is rejected by GitHub with 422 "line must be part of the diff".
//   - Deleted lines must be anchored with side=LEFT and the *old* line number.
//   - The position counter keeps increasing across hunk headers within a file
//     and resets at the start of each new file.
package diff

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// Side identifies which revision of the file a line belongs to.
type Side string

const (
	SideRight Side = "RIGHT" // the head revision (added or context lines)
	SideLeft  Side = "LEFT"  // the base revision (deleted lines)
)

// Anchor is a location a review comment can legally be attached to.
type Anchor struct {
	Line     int  // line number in the revision named by Side
	Side     Side // RIGHT for added/context, LEFT for deleted
	Position int  // legacy diff offset, 1-based from the first @@ of the file
	Added    bool // true when the line was introduced by this PR
}

// Hunk is one @@ block.
type Hunk struct {
	Header   string
	OldStart int
	OldLines int
	NewStart int
	NewLines int
	Body     []string // raw diff lines, including the leading +/-/space
}

// File is one file's worth of diff.
type File struct {
	OldPath  string
	NewPath  string
	IsBinary bool
	IsRename bool
	Hunks    []Hunk

	// anchors maps "SIDE:line" to the anchor metadata for that line.
	anchors map[string]Anchor
}

// Path returns the path to use when addressing the file in the GitHub API,
// which is always the head-revision path except for pure deletions.
func (f *File) Path() string {
	if f.NewPath != "" && f.NewPath != "/dev/null" {
		return f.NewPath
	}
	return f.OldPath
}

// Anchor resolves a (side, line) pair to a legal comment location. The second
// return value is false when that line is not part of the diff, which is the
// single most common cause of a 422 from the review API.
func (f *File) Anchor(side Side, line int) (Anchor, bool) {
	a, ok := f.anchors[anchorKey(side, line)]
	return a, ok
}

// AddedLines returns every line this PR introduced, in file order. These are
// the lines a reviewer is normally allowed to comment on.
func (f *File) AddedLines() []Anchor {
	var out []Anchor
	for _, h := range f.Hunks {
		newLine := h.NewStart
		for _, l := range h.Body {
			switch {
			case strings.HasPrefix(l, "+"):
				if a, ok := f.Anchor(SideRight, newLine); ok {
					out = append(out, a)
				}
				newLine++
			case strings.HasPrefix(l, "-"):
				// no new-side line consumed
			default:
				newLine++
			}
		}
	}
	return out
}

// NearestAddedLine snaps an arbitrary line number to the closest line the PR
// actually touched in this file. The LLM routinely reports a line that is one
// or two off, because it reasons about the file while GitHub only accepts a
// comment on a line that is part of the diff. Dropping those findings loses
// real defects over a coordinate error, so within a small tolerance they are
// snapped instead; see docs/RESULTS.md for what that is worth in practice.
//
// tolerance caps how far we are willing to move a finding. Beyond that the
// finding is reported at file level instead of inline.
func (f *File) NearestAddedLine(line, tolerance int) (Anchor, bool) {
	best, found := Anchor{}, false
	bestDist := tolerance + 1
	for _, a := range f.AddedLines() {
		d := a.Line - line
		if d < 0 {
			d = -d
		}
		if d < bestDist {
			best, bestDist, found = a, d, true
		}
	}
	return best, found
}

// Parse turns a unified diff into per-file structures.
func Parse(raw string) ([]*File, error) {
	var files []*File
	var cur *File
	var curHunk *Hunk
	// position counts diff lines from the first @@ header of the current file.
	position := 0
	oldLine, newLine := 0, 0

	flushHunk := func() {
		if cur != nil && curHunk != nil {
			cur.Hunks = append(cur.Hunks, *curHunk)
			curHunk = nil
		}
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			files = append(files, cur)
			cur = nil
		}
	}

	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()

		switch {
		case strings.HasPrefix(line, "diff --git "):
			flushFile()
			cur = &File{anchors: map[string]Anchor{}}
			position = 0
			old, new_, ok := parseDiffGitHeader(line)
			if ok {
				cur.OldPath, cur.NewPath = old, new_
			}

		case cur == nil:
			// Preamble before the first file header (commit message, etc.).
			continue

		case strings.HasPrefix(line, "--- "):
			cur.OldPath = stripPathPrefix(strings.TrimPrefix(line, "--- "))

		case strings.HasPrefix(line, "+++ "):
			cur.NewPath = stripPathPrefix(strings.TrimPrefix(line, "+++ "))

		case strings.HasPrefix(line, "rename from ") || strings.HasPrefix(line, "rename to "):
			cur.IsRename = true

		case strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch"):
			cur.IsBinary = true

		case strings.HasPrefix(line, "@@"):
			flushHunk()
			h, err := parseHunkHeader(line)
			if err != nil {
				return nil, fmt.Errorf("file %s: %w", cur.Path(), err)
			}
			curHunk = &h
			oldLine, newLine = h.OldStart, h.NewStart
			// The hunk header itself occupies a position slot for every hunk
			// after the first; the first "@@" is position 0 and the line below
			// it is position 1.
			if position > 0 {
				position++
			}

		case curHunk != nil:
			curHunk.Body = append(curHunk.Body, line)
			position++
			switch {
			case strings.HasPrefix(line, "+"):
				cur.anchors[anchorKey(SideRight, newLine)] = Anchor{
					Line: newLine, Side: SideRight, Position: position, Added: true,
				}
				newLine++
			case strings.HasPrefix(line, "-"):
				cur.anchors[anchorKey(SideLeft, oldLine)] = Anchor{
					Line: oldLine, Side: SideLeft, Position: position,
				}
				oldLine++
			case strings.HasPrefix(line, "\\"):
				// "\ No newline at end of file" — carries no line number and,
				// counter-intuitively, still consumes a diff position.
			default: // context line
				cur.anchors[anchorKey(SideRight, newLine)] = Anchor{
					Line: newLine, Side: SideRight, Position: position,
				}
				oldLine++
				newLine++
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	flushFile()
	return files, nil
}

// Stats summarises a parsed diff for logging and for the LLM prompt header.
type Stats struct {
	Files     int
	Additions int
	Deletions int
}

func Summarize(files []*File) Stats {
	s := Stats{Files: len(files)}
	for _, f := range files {
		for _, h := range f.Hunks {
			for _, l := range h.Body {
				switch {
				case strings.HasPrefix(l, "+"):
					s.Additions++
				case strings.HasPrefix(l, "-"):
					s.Deletions++
				}
			}
		}
	}
	return s
}

// Render rebuilds a compact, line-numbered view of the diff for the LLM. Real
// diffs are noisy; annotating each added line with its true file line number
// is what lets the model cite a line we can actually anchor a comment to.
func Render(files []*File, maxBytes int) string {
	var b strings.Builder
	for _, f := range files {
		if f.IsBinary {
			fmt.Fprintf(&b, "\n### %s (binary, skipped)\n", f.Path())
			continue
		}
		fmt.Fprintf(&b, "\n### %s\n", f.Path())
		for _, h := range f.Hunks {
			fmt.Fprintf(&b, "%s\n", h.Header)
			newLine := h.NewStart
			for _, l := range h.Body {
				switch {
				case strings.HasPrefix(l, "+"):
					fmt.Fprintf(&b, "%6d + %s\n", newLine, strings.TrimPrefix(l, "+"))
					newLine++
				case strings.HasPrefix(l, "-"):
					fmt.Fprintf(&b, "       - %s\n", strings.TrimPrefix(l, "-"))
				case strings.HasPrefix(l, "\\"):
				default:
					fmt.Fprintf(&b, "%6d   %s\n", newLine, strings.TrimPrefix(l, " "))
					newLine++
				}
			}
			if b.Len() > maxBytes {
				b.WriteString("\n... diff truncated (exceeded MAX_DIFF_BYTES) ...\n")
				return b.String()
			}
		}
	}
	return b.String()
}

func anchorKey(side Side, line int) string {
	return string(side) + ":" + strconv.Itoa(line)
}

func parseDiffGitHeader(line string) (old, new_ string, ok bool) {
	rest := strings.TrimPrefix(line, "diff --git ")
	// Paths may contain spaces; the common case is "a/x b/x". Split on " b/"
	// which is unambiguous for git-generated headers.
	idx := strings.Index(rest, " b/")
	if idx < 0 {
		return "", "", false
	}
	return stripPathPrefix(rest[:idx]), stripPathPrefix(rest[idx+1:]), true
}

func stripPathPrefix(p string) string {
	p = strings.TrimSpace(p)
	// Drop a trailing tab-separated timestamp that some diff tools append.
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	if p == "/dev/null" {
		return p
	}
	for _, pre := range []string{"a/", "b/", "i/", "w/", "c/", "o/"} {
		if strings.HasPrefix(p, pre) {
			return p[len(pre):]
		}
	}
	return p
}

// parseHunkHeader reads "@@ -oldStart,oldLines +newStart,newLines @@ context".
// The ",n" part is omitted by git when the count is 1.
func parseHunkHeader(line string) (Hunk, error) {
	h := Hunk{Header: line, OldLines: 1, NewLines: 1}
	end := strings.Index(line[2:], "@@")
	if end < 0 {
		return h, fmt.Errorf("malformed hunk header %q", line)
	}
	spec := strings.TrimSpace(line[2 : 2+end])
	for _, part := range strings.Fields(spec) {
		if len(part) < 2 {
			continue
		}
		sign, nums := part[0], part[1:]
		start, count := 0, 1
		if i := strings.IndexByte(nums, ','); i >= 0 {
			start, _ = strconv.Atoi(nums[:i])
			count, _ = strconv.Atoi(nums[i+1:])
		} else {
			start, _ = strconv.Atoi(nums)
		}
		switch sign {
		case '-':
			h.OldStart, h.OldLines = start, count
		case '+':
			h.NewStart, h.NewLines = start, count
		}
	}
	if h.OldStart == 0 && h.NewStart == 0 {
		return h, fmt.Errorf("malformed hunk header %q", line)
	}
	return h, nil
}
