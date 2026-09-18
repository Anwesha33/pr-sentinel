// Package agent runs the review: a bounded tool-calling loop over the pull
// request, followed by a structured extraction pass, followed by validation of
// every finding against the actual diff.
//
// The loop and the extraction are deliberately two separate LLM phases:
//
//	phase 1 (tools on):  investigate. The model reads files, greps, runs tests.
//	                     Its prose here is scratch work and is never shown.
//	phase 2 (tools off): emit findings as JSON, forced by responseMimeType.
//
// They are split because the Gemini API refuses to combine function calling
// with forced JSON output, and because mixing "think" and "format" in one call
// measurably degraded both: the model either stopped calling tools to hurry to
// the JSON, or produced JSON with tool-call fragments embedded in it.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Anwesha33/pr-sentinel/internal/analysis"
	"github.com/Anwesha33/pr-sentinel/internal/diff"
	"github.com/Anwesha33/pr-sentinel/internal/llm"
	"github.com/Anwesha33/pr-sentinel/internal/store"
)

const systemPrompt = `You are a senior backend engineer reviewing a pull request. You are reviewing for the author's benefit, not to demonstrate thoroughness.

WHAT TO REPORT
Report only defects a competent reviewer would block or question:
- correctness: logic errors, off-by-one, wrong operator, inverted condition, unhandled error path, incorrect nil/empty handling
- concurrency: data races, unsynchronised shared state, deadlock, goroutine or connection leaks
- resource handling: unclosed files/connections/transactions, unbounded growth, missing timeouts
- security: injection, missing authz check, secret in code or log, unsafe deserialisation, path traversal
- data integrity: non-idempotent writes, lost updates, missing transaction boundary
- api/contract: breaking change to a signature, response shape, or persisted format

WHAT NOT TO REPORT
- style, naming, formatting, import order — a linter owns these
- "consider adding a test" with no specific untested branch named
- "consider adding a comment", "this could be extracted", generic refactoring advice
- anything you cannot tie to a specific line the pull request changed
- speculation about code you did not read. If a call's behaviour matters, read it with read_file first

HOW TO WORK
You have tools. Use them before concluding anything about code the diff does not show. A finding that rests on an assumption about an unread function is worth less than no finding at all. The diff you are given annotates each added line with its real line number in the file — cite those numbers.

Be concrete. "This can panic when items is empty because len(items)-1 is -1" is a review. "Consider handling edge cases" is noise.

Work through the change, then stop calling tools and briefly state what you found. A pull request with no real defects is a normal and valuable outcome — say so rather than inventing something.`

const extractionPrompt = `Convert your review into JSON. Output a single object of this exact shape and nothing else:

{
  "summary": "two or three sentences for the review body: what this PR does and your overall assessment",
  "verdict": "approve" | "comment" | "request_changes",
  "findings": [
    {
      "file": "repository-relative path exactly as it appears in the diff",
      "line": 123,
      "severity": "critical" | "major" | "minor",
      "category": "correctness" | "concurrency" | "resource" | "security" | "data-integrity" | "api-contract",
      "title": "one line, under 80 characters",
      "body": "what is wrong, why it is wrong, and the concrete fix. Include a short code suggestion when the fix is small.",
      "confidence": 0.0
    }
  ]
}

Rules:
- "line" MUST be a line number that appears in the diff you were given, on a line the PR added or changed.
- confidence is your honest probability that this is a real defect a maintainer would act on. Below 0.5 means do not include it.
- If there are no real defects, return an empty findings array and verdict "approve".
- Do not restate the same defect twice.`

// Input is everything the agent needs to review one pull request.
type Input struct {
	Owner       string
	Repo        string
	PRNumber    int
	Title       string
	Description string
	HeadSHA     string
	BaseRef     string

	Files        []*diff.File
	RenderedDiff string
	StaticReport string
	Toolchain    analysis.Toolchain
}

// Result is the reviewed outcome.
type Result struct {
	Summary  string
	Verdict  string
	Findings []store.Finding
	// Dropped carries findings that were rejected during validation, kept for
	// the evaluation harness and for debugging a disappointing review.
	Dropped    []store.Finding
	Usage      store.Usage
	Steps      []store.Step
	Transcript string
}

// StepHook is called after each tool invocation so the caller can persist the
// audit trail as it happens rather than only at the end.
type StepHook func(store.Step)

type Agent struct {
	LLM      *llm.Client
	Tools    *Toolbox
	MaxSteps int
	// LineTolerance is how far a reported line may be snapped to the nearest
	// line the PR actually touched. 0 disables snapping.
	LineTolerance int
	// MinConfidence drops findings the model itself is unsure about.
	MinConfidence float64
	Attempt       int
	OnStep        StepHook
}

func (a *Agent) Review(ctx context.Context, in Input) (*Result, error) {
	start := time.Now()
	res := &Result{Verdict: "comment"}

	contents := []llm.Content{{
		Role:  llm.RoleUser,
		Parts: []llm.Part{{Text: buildOpeningMessage(in)}},
	}}

	maxSteps := a.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 10
	}

	// ---- phase 1: investigate with tools -------------------------------
	var transcript strings.Builder
	step := 0
	for step < maxSteps {
		resp, err := a.LLM.Generate(ctx, llm.Request{
			System:   systemPrompt,
			Contents: contents,
			Tools:    ToolDeclarations(),
			Config:   llm.GenerationConfig{Temperature: 0.1},
		})
		if err != nil {
			return nil, fmt.Errorf("investigation step %d: %w", step+1, err)
		}
		res.Usage.PromptTokens += resp.Usage.PromptTokens
		res.Usage.OutputTokens += resp.Usage.OutputTokens
		res.Usage.LLMCalls++

		if resp.Text != "" {
			transcript.WriteString(resp.Text)
			transcript.WriteString("\n")
		}

		if len(resp.FunctionCalls) == 0 {
			// The model is done investigating.
			contents = append(contents, resp.RawContent)
			break
		}

		// Echo the model's turn back exactly as it arrived. Rebuilding an
		// equivalent-looking Content is not enough: the API requires both the
		// functionCall parts and their opaque thought signatures to be present
		// in history, and drops the request with 400 if a signature is missing.
		contents = append(contents, resp.RawContent)

		responseParts := make([]llm.Part, 0, len(resp.FunctionCalls))
		for _, call := range resp.FunctionCalls {
			step++
			callStart := time.Now()
			outcome := a.Tools.Execute(ctx, call)
			st := store.Step{
				Attempt:    a.Attempt,
				StepNo:     step,
				ToolName:   call.Name,
				ToolArgs:   call.Args,
				ResultSize: outcome.Size,
				ResultHead: outcome.Summary,
				Error:      outcome.Err,
				LatencyMS:  time.Since(callStart).Milliseconds(),
				CacheHit:   outcome.CacheHit,
			}
			res.Steps = append(res.Steps, st)
			if a.OnStep != nil {
				a.OnStep(st)
			}
			fmt.Fprintf(&transcript, "[tool] %s(%s) -> %s\n", call.Name, compactArgs(call.Args), outcome.Summary)

			responseParts = append(responseParts, llm.Part{FunctionResponse: &llm.FunctionResponse{
				Name:     call.Name,
				Response: outcome.Payload,
			}})
		}
		contents = append(contents, llm.Content{Role: llm.RoleUser, Parts: responseParts})

		if step >= maxSteps {
			// Tell the model why it is being cut off, so its final answer is a
			// conclusion rather than a half-formed plan.
			contents = append(contents, llm.Content{
				Role: llm.RoleUser,
				Parts: []llm.Part{{Text: fmt.Sprintf(
					"You have used your budget of %d tool calls. Stop investigating and state your findings based on what you have already read.", maxSteps)}},
			})
			final, err := a.LLM.Generate(ctx, llm.Request{
				System: systemPrompt, Contents: contents,
				Config: llm.GenerationConfig{Temperature: 0.1},
			})
			if err == nil {
				res.Usage.PromptTokens += final.Usage.PromptTokens
				res.Usage.OutputTokens += final.Usage.OutputTokens
				res.Usage.LLMCalls++
				transcript.WriteString(final.Text)
				contents = append(contents, final.RawContent)
			}
			break
		}
	}
	res.Transcript = transcript.String()

	// ---- phase 2: structured extraction (no tools, forced JSON) ---------
	extractContents := append(contents, llm.Content{
		Role: llm.RoleUser, Parts: []llm.Part{{Text: extractionPrompt}},
	})
	structured, err := a.LLM.Generate(ctx, llm.Request{
		System:    systemPrompt,
		Contents:  extractContents,
		ForceJSON: true,
		Config:    llm.GenerationConfig{Temperature: 0},
	})
	if err != nil {
		return nil, fmt.Errorf("extraction: %w", err)
	}
	res.Usage.PromptTokens += structured.Usage.PromptTokens
	res.Usage.OutputTokens += structured.Usage.OutputTokens
	res.Usage.LLMCalls++

	parsed, err := parseExtraction(structured.Text)
	if err != nil {
		return nil, fmt.Errorf("extraction returned unusable JSON: %w", err)
	}
	res.Summary = strings.TrimSpace(parsed.Summary)
	if parsed.Verdict != "" {
		res.Verdict = parsed.Verdict
	}

	// ---- phase 3: validate every finding against the real diff ----------
	kept, dropped := a.validate(parsed.Findings, in.Files)
	res.Findings, res.Dropped = kept, dropped
	res.Usage.DurationMS = time.Since(start).Milliseconds()
	return res, nil
}

// rawFinding is a finding as the model emitted it, before any validation.
type rawFinding struct {
	File       string  `json:"file"`
	Line       int     `json:"line"`
	Severity   string  `json:"severity"`
	Category   string  `json:"category"`
	Title      string  `json:"title"`
	Body       string  `json:"body"`
	Confidence float64 `json:"confidence"`
}

type extraction struct {
	Summary  string       `json:"summary"`
	Verdict  string       `json:"verdict"`
	Findings []rawFinding `json:"findings"`
}

func parseExtraction(text string) (*extraction, error) {
	raw := text
	if j, ok := llm.ExtractJSON(text); ok {
		raw = j
	}
	var e extraction
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return nil, fmt.Errorf("%w (got %.200q)", err, text)
	}
	return &e, nil
}

// validate is the guard between "the model said something" and "we posted it".
//
// Every finding must name a file in the diff and a line the PR touched. A
// finding that fails is not silently discarded: it is kept in Dropped with a
// reason, which is what makes the failure modes measurable in eval/.
func (a *Agent) validate(raw []rawFinding, files []*diff.File) (kept, dropped []store.Finding) {

	byPath := map[string]*diff.File{}
	for _, f := range files {
		byPath[f.Path()] = f
	}

	seen := map[string]bool{}
	for _, r := range raw {
		f := store.Finding{
			FilePath:   strings.TrimSpace(r.File),
			Line:       r.Line,
			Side:       string(diff.SideRight),
			Severity:   normalizeSeverity(r.Severity),
			Category:   strings.TrimSpace(strings.ToLower(r.Category)),
			Title:      strings.TrimSpace(r.Title),
			Body:       strings.TrimSpace(r.Body),
			Confidence: r.Confidence,
		}
		if f.Title == "" || f.Body == "" {
			f.DroppedReason = "empty title or body"
			dropped = append(dropped, f)
			continue
		}
		if f.Confidence < a.MinConfidence {
			f.DroppedReason = fmt.Sprintf("confidence %.2f below threshold %.2f", f.Confidence, a.MinConfidence)
			dropped = append(dropped, f)
			continue
		}

		// Deduplicate on file+line+title: the two-phase design occasionally
		// restates a finding the investigation phase already made.
		dedupeKey := strings.ToLower(f.FilePath + "|" + fmt.Sprint(f.Line) + "|" + f.Title)
		if seen[dedupeKey] {
			f.DroppedReason = "duplicate of an earlier finding"
			dropped = append(dropped, f)
			continue
		}
		seen[dedupeKey] = true

		df, ok := byPath[f.FilePath]
		if !ok {
			// Models sometimes prefix with the repo name or drop a directory.
			if alt, ok2 := matchPathLoosely(f.FilePath, byPath); ok2 {
				df, ok = alt, true
				f.FilePath = alt.Path()
			}
		}
		if !ok {
			f.DroppedReason = "file is not part of this pull request's diff"
			f.Anchored = false
			dropped = append(dropped, f)
			continue
		}

		if anchor, exact := df.Anchor(diff.SideRight, f.Line); exact && anchor.Added {
			f.Position = anchor.Position
			f.Anchored = true
		} else if snapped, ok := df.NearestAddedLine(f.Line, a.LineTolerance); ok && a.LineTolerance > 0 {
			f.Line = snapped.Line
			f.Position = snapped.Position
			f.Anchored = true
		} else {
			// Keep it, but report it in the summary instead of inline. The
			// finding may still be correct; only its coordinates are wrong.
			f.Anchored = false
			f.DroppedReason = "line not in diff; reported at file level"
		}
		kept = append(kept, f)
	}

	sort.SliceStable(kept, func(i, j int) bool {
		if severityRank(kept[i].Severity) != severityRank(kept[j].Severity) {
			return severityRank(kept[i].Severity) < severityRank(kept[j].Severity)
		}
		if kept[i].FilePath != kept[j].FilePath {
			return kept[i].FilePath < kept[j].FilePath
		}
		return kept[i].Line < kept[j].Line
	})
	return kept, dropped
}

func matchPathLoosely(want string, byPath map[string]*diff.File) (*diff.File, bool) {
	want = strings.TrimPrefix(strings.TrimSpace(want), "./")
	for p, f := range byPath {
		if strings.EqualFold(p, want) ||
			strings.HasSuffix(p, "/"+want) ||
			strings.HasSuffix(want, "/"+p) {
			return f, true
		}
	}
	return nil, false
}

func normalizeSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical", "blocker", "high":
		return "critical"
	case "major", "medium":
		return "major"
	case "minor", "low", "nit", "info":
		return "minor"
	default:
		return "major"
	}
}

func severityRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "major":
		return 1
	default:
		return 2
	}
}

func buildOpeningMessage(in Input) string {
	stats := diff.Summarize(in.Files)
	var b strings.Builder
	fmt.Fprintf(&b, "Repository: %s/%s (toolchain: %s)\n", in.Owner, in.Repo, in.Toolchain)
	fmt.Fprintf(&b, "Pull request #%d: %s\n", in.PRNumber, in.Title)
	fmt.Fprintf(&b, "Target branch: %s, head commit: %s\n", in.BaseRef, in.HeadSHA)
	fmt.Fprintf(&b, "Size: %d files, +%d/-%d lines\n\n", stats.Files, stats.Additions, stats.Deletions)

	if d := strings.TrimSpace(in.Description); d != "" {
		fmt.Fprintf(&b, "Author's description:\n%s\n\n", truncate(d, 3000))
	}

	fmt.Fprintf(&b, "Static analysis and build results already collected for you:\n%s\n", in.StaticReport)

	b.WriteString("\nThe diff follows. Numbers in the left column are real line numbers in the " +
		"head revision; '+' marks a line this pull request added. Only lines shown here can " +
		"carry an inline comment.\n")
	b.WriteString(in.RenderedDiff)
	b.WriteString("\n\nInvestigate anything you need with the tools, then give your review.")
	return b.String()
}

// RenderReviewBody builds the markdown posted as the review's summary comment.
func RenderReviewBody(res *Result, model string, dryRun bool) string {
	var b strings.Builder
	b.WriteString("## pr-sentinel review\n\n")
	if res.Summary != "" {
		b.WriteString(res.Summary)
		b.WriteString("\n\n")
	}

	counts := map[string]int{}
	for _, f := range res.Findings {
		counts[f.Severity]++
	}
	if len(res.Findings) == 0 {
		b.WriteString("No blocking defects found in the changed lines.\n\n")
	} else {
		fmt.Fprintf(&b, "**%d finding(s)** — %d critical, %d major, %d minor.\n\n",
			len(res.Findings), counts["critical"], counts["major"], counts["minor"])
	}

	// Findings that could not be anchored inline are listed here so they are
	// not lost.
	var unanchored []store.Finding
	for _, f := range res.Findings {
		if !f.Anchored {
			unanchored = append(unanchored, f)
		}
	}
	if len(unanchored) > 0 {
		b.WriteString("### Findings that could not be attached to a diff line\n\n")
		for _, f := range unanchored {
			fmt.Fprintf(&b, "- **%s** `%s:%d` — %s\n\n  %s\n\n",
				strings.ToUpper(f.Severity), f.FilePath, f.Line, f.Title, f.Body)
		}
	}

	b.WriteString("---\n")
	fmt.Fprintf(&b, "<sub>Generated by pr-sentinel using `%s` — %d LLM call(s), %d tool call(s), %d prompt + %d output tokens.",
		model, res.Usage.LLMCalls, len(res.Steps), res.Usage.PromptTokens, res.Usage.OutputTokens)
	if dryRun {
		b.WriteString(" **Dry run: this review was computed but not published.**")
	}
	b.WriteString("</sub>\n")
	return b.String()
}

// RenderInlineComment formats one finding as an inline review comment.
func RenderInlineComment(f store.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s · %s** — %s\n\n", strings.ToUpper(f.Severity), f.Category, f.Title)
	b.WriteString(f.Body)
	fmt.Fprintf(&b, "\n\n<sub>pr-sentinel · confidence %.2f</sub>", f.Confidence)
	return b.String()
}

func compactArgs(args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return "?"
	}
	return truncate(string(raw), 160)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
