// Command evalrunner scores the review agent against fixtures with known
// defects.
//
// Why this exists: "the LLM reviews pull requests" is an unfalsifiable claim.
// Each fixture is a small pull request with a planted defect (or deliberately
// no defect at all), so the same agent code that runs in production can be
// measured for precision, recall, cost and latency, and a prompt change can be
// shown to help or hurt rather than argued about.
//
// Scoring: a finding counts as a true positive when it names the right file and
// lands within LineTolerance lines of the planted defect. Each planted defect
// can be claimed by at most one finding; every other finding is a false
// positive. The clean fixture has no defects, so every finding it draws is a
// false positive — that fixture alone measures the failure mode that gets a
// review bot muted by its team.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Anwesha33/pr-sentinel/internal/agent"
	"github.com/Anwesha33/pr-sentinel/internal/analysis"
	"github.com/Anwesha33/pr-sentinel/internal/config"
	"github.com/Anwesha33/pr-sentinel/internal/diff"
	"github.com/Anwesha33/pr-sentinel/internal/llm"
	"github.com/Anwesha33/pr-sentinel/internal/store"
	"github.com/Anwesha33/pr-sentinel/internal/workspace"
)

// Gemini 3.6 Flash list price, USD per million tokens. Override with
// -price-in / -price-out when the model changes.
const (
	defaultPriceInPerM  = 0.10
	defaultPriceOutPerM = 0.40
)

type expectedBug struct {
	File        string `json:"file"`
	Anchor      string `json:"anchor"`
	Line        int    `json:"line"`
	Category    string `json:"category"`
	Description string `json:"description"`
}

type fixture struct {
	Dir      string
	Name     string        `json:"name"`
	Language string        `json:"language"`
	Bugs     []expectedBug `json:"bugs"`
	Note     string        `json:"note"`
}

type fixtureResult struct {
	Fixture        string          `json:"fixture"`
	Name           string          `json:"name"`
	PlantedDefects int             `json:"planted_defects"`
	Reported       int             `json:"reported_findings"`
	TruePositives  int             `json:"true_positives"`
	FalsePositives int             `json:"false_positives"`
	FalseNegatives int             `json:"false_negatives"`
	Missed         []string        `json:"missed,omitempty"`
	Spurious       []string        `json:"spurious,omitempty"`
	Matched        []string        `json:"matched,omitempty"`
	ToolCalls      int             `json:"tool_calls"`
	LLMCalls       int             `json:"llm_calls"`
	PromptTokens   int             `json:"prompt_tokens"`
	OutputTokens   int             `json:"output_tokens"`
	CostUSD        float64         `json:"cost_usd"`
	DurationMS     int64           `json:"duration_ms"`
	Verdict        string          `json:"verdict"`
	Findings       []store.Finding `json:"findings"`
	Error          string          `json:"error,omitempty"`
}

type report struct {
	Model         string          `json:"model"`
	RanAt         time.Time       `json:"ran_at"`
	LineTolerance int             `json:"line_tolerance"`
	MinConfidence float64         `json:"min_confidence"`
	MaxSteps      int             `json:"max_agent_steps"`
	Fixtures      []fixtureResult `json:"fixtures"`
	Totals        struct {
		PlantedDefects int     `json:"planted_defects"`
		TruePositives  int     `json:"true_positives"`
		FalsePositives int     `json:"false_positives"`
		FalseNegatives int     `json:"false_negatives"`
		Precision      float64 `json:"precision"`
		Recall         float64 `json:"recall"`
		F1             float64 `json:"f1"`
		CleanFixtureFP int     `json:"clean_fixture_false_positives"`
		TotalCostUSD   float64 `json:"total_cost_usd"`
		MeanCostUSD    float64 `json:"mean_cost_per_review_usd"`
		MeanDurationMS int64   `json:"mean_duration_ms"`
		MeanToolCalls  float64 `json:"mean_tool_calls"`
	} `json:"totals"`
}

func main() {
	var (
		fixturesDir = flag.String("fixtures", "eval/fixtures", "directory of fixtures")
		only        = flag.String("only", "", "run only fixtures whose directory name contains this string")
		outPath     = flag.String("out", "eval/report.json", "where to write the JSON report")
		tolerance   = flag.Int("tolerance", 3, "how many lines a finding may be away from the planted defect")
		minConf     = flag.Float64("min-confidence", 0.5, "drop findings below this confidence")
		maxSteps    = flag.Int("max-steps", 12, "tool-call budget per review")
		priceIn     = flag.Float64("price-in", defaultPriceInPerM, "USD per million prompt tokens")
		priceOut    = flag.Float64("price-out", defaultPriceOutPerM, "USD per million output tokens")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		fatal("config: %v", err)
	}
	if cfg.GeminiAPIKey == "" {
		fatal("GEMINI_API_KEY is not set")
	}

	fixtures, err := loadFixtures(*fixturesDir, *only)
	if err != nil {
		fatal("load fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		fatal("no fixtures matched")
	}

	client := llm.New(cfg.GeminiAPIKey, cfg.GeminiModel, cfg.LLMTimeout)
	rep := report{
		Model: cfg.GeminiModel, RanAt: time.Now(),
		LineTolerance: *tolerance, MinConfidence: *minConf, MaxSteps: *maxSteps,
	}

	fmt.Printf("pr-sentinel evaluation — model %s, %d fixtures\n\n", cfg.GeminiModel, len(fixtures))

	for _, fx := range fixtures {
		fmt.Printf("  %-22s ", filepath.Base(fx.Dir))
		res := runFixture(context.Background(), client, cfg, fx, *tolerance, *minConf, *maxSteps, *priceIn, *priceOut)
		if res.Error != "" {
			fmt.Printf("ERROR: %s\n", res.Error)
		} else {
			fmt.Printf("TP %d  FP %d  FN %d   %d tools, %5.2fs, $%.5f\n",
				res.TruePositives, res.FalsePositives, res.FalseNegatives,
				res.ToolCalls, float64(res.DurationMS)/1000, res.CostUSD)
		}
		rep.Fixtures = append(rep.Fixtures, res)
	}

	summarize(&rep)
	printSummary(&rep)

	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		fatal("create report dir: %v", err)
	}
	raw, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(*outPath, raw, 0o644); err != nil {
		fatal("write report: %v", err)
	}
	fmt.Printf("\nreport written to %s\n", *outPath)
}

func runFixture(ctx context.Context, client *llm.Client, cfg *config.Config, fx fixture,
	tolerance int, minConf float64, maxSteps int, priceIn, priceOut float64) fixtureResult {

	res := fixtureResult{Fixture: filepath.Base(fx.Dir), Name: fx.Name, PlantedDefects: len(fx.Bugs)}

	rawDiff, err := os.ReadFile(filepath.Join(fx.Dir, "pr.diff"))
	if err != nil {
		res.Error = err.Error()
		return res
	}
	files, err := diff.Parse(string(rawDiff))
	if err != nil {
		res.Error = "parse diff: " + err.Error()
		return res
	}

	head := filepath.Join(fx.Dir, "head")
	ws := workspace.Open(head)
	runner := analysis.NewRunner(head)
	// The fixtures are tiny; a long toolchain timeout would only hide a hang.
	runner.Timeout = 60 * time.Second

	staticResults := runner.StaticAnalysis(ctx)

	ag := &agent.Agent{
		LLM:           client,
		Tools:         &agent.Toolbox{WS: ws, Runner: runner, MaxFileBytes: cfg.MaxFileReadBytes},
		MaxSteps:      maxSteps,
		LineTolerance: tolerance,
		MinConfidence: minConf,
		Attempt:       1,
	}

	out, err := ag.Review(ctx, agent.Input{
		Owner: "eval", Repo: res.Fixture, PRNumber: 1,
		Title:        fx.Name,
		Description:  "Evaluation fixture.",
		HeadSHA:      "fixture",
		BaseRef:      "main",
		Files:        files,
		RenderedDiff: diff.Render(files, cfg.MaxDiffBytes),
		StaticReport: analysis.Summarize(staticResults),
		Toolchain:    analysis.Detect(head),
	})
	if err != nil {
		res.Error = err.Error()
		return res
	}

	res.Findings = out.Findings
	res.Reported = len(out.Findings)
	res.ToolCalls = len(out.Steps)
	res.LLMCalls = out.Usage.LLMCalls
	res.PromptTokens = out.Usage.PromptTokens
	res.OutputTokens = out.Usage.OutputTokens
	res.DurationMS = out.Usage.DurationMS
	res.Verdict = out.Verdict
	res.CostUSD = float64(out.Usage.PromptTokens)/1e6*priceIn + float64(out.Usage.OutputTokens)/1e6*priceOut

	score(&res, fx, out.Findings, head, tolerance)
	return res
}

// score matches findings to planted defects greedily by distance.
func score(res *fixtureResult, fx fixture, findings []store.Finding, headDir string, tolerance int) {
	type target struct {
		bug   expectedBug
		line  int
		taken bool
	}
	targets := make([]target, 0, len(fx.Bugs))
	for _, b := range fx.Bugs {
		line := b.Line
		if b.Anchor != "" {
			if n, ok := resolveAnchor(filepath.Join(headDir, b.File), b.Anchor); ok {
				line = n
			}
		}
		targets = append(targets, target{bug: b, line: line})
	}

	claimed := make([]bool, len(findings))
	// Nearest-first so a finding that is exactly on the line is not stolen by a
	// defect it is merely near.
	for ti := range targets {
		bestIdx, bestDist := -1, tolerance+1
		for fi, f := range findings {
			if claimed[fi] || !sameFile(f.FilePath, targets[ti].bug.File) {
				continue
			}
			d := f.Line - targets[ti].line
			if d < 0 {
				d = -d
			}
			if d <= tolerance && d < bestDist {
				bestIdx, bestDist = fi, d
			}
		}
		if bestIdx >= 0 {
			claimed[bestIdx] = true
			targets[ti].taken = true
			res.TruePositives++
			res.Matched = append(res.Matched, fmt.Sprintf("%s:%d %s (reported: %s)",
				targets[ti].bug.File, targets[ti].line, targets[ti].bug.Category, findings[bestIdx].Title))
		}
	}

	for ti := range targets {
		if !targets[ti].taken {
			res.FalseNegatives++
			res.Missed = append(res.Missed, fmt.Sprintf("%s:%d %s — %s",
				targets[ti].bug.File, targets[ti].line, targets[ti].bug.Category,
				truncate(targets[ti].bug.Description, 120)))
		}
	}
	for fi, f := range findings {
		if !claimed[fi] {
			res.FalsePositives++
			res.Spurious = append(res.Spurious, fmt.Sprintf("%s:%d [%s/%s] %s",
				f.FilePath, f.Line, f.Severity, f.Category, f.Title))
		}
	}
}

// resolveAnchor finds the 1-based line of a unique code snippet in the head
// revision. Fixtures name defects by the code itself so that editing a fixture
// cannot silently invalidate a hard-coded line number.
func resolveAnchor(path, anchor string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, anchor) {
			return i + 1, true
		}
	}
	return 0, false
}

func sameFile(a, b string) bool {
	a, b = strings.TrimPrefix(a, "./"), strings.TrimPrefix(b, "./")
	return a == b || strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
}

func loadFixtures(dir, only string) ([]fixture, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []fixture
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if only != "" && !strings.Contains(e.Name(), only) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), "expected.json"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		var fx fixture
		if err := json.Unmarshal(raw, &fx); err != nil {
			return nil, fmt.Errorf("%s/expected.json: %w", e.Name(), err)
		}
		fx.Dir = filepath.Join(dir, e.Name())
		out = append(out, fx)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

func summarize(rep *report) {
	var totalDuration int64
	var totalTools int
	counted := 0
	for _, f := range rep.Fixtures {
		if f.Error != "" {
			continue
		}
		counted++
		rep.Totals.PlantedDefects += f.PlantedDefects
		rep.Totals.TruePositives += f.TruePositives
		rep.Totals.FalsePositives += f.FalsePositives
		rep.Totals.FalseNegatives += f.FalseNegatives
		rep.Totals.TotalCostUSD += f.CostUSD
		totalDuration += f.DurationMS
		totalTools += f.ToolCalls
		if f.PlantedDefects == 0 {
			rep.Totals.CleanFixtureFP += f.FalsePositives
		}
	}
	if tp, fp := rep.Totals.TruePositives, rep.Totals.FalsePositives; tp+fp > 0 {
		rep.Totals.Precision = float64(tp) / float64(tp+fp)
	}
	if tp, fn := rep.Totals.TruePositives, rep.Totals.FalseNegatives; tp+fn > 0 {
		rep.Totals.Recall = float64(tp) / float64(tp+fn)
	}
	if p, r := rep.Totals.Precision, rep.Totals.Recall; p+r > 0 {
		rep.Totals.F1 = 2 * p * r / (p + r)
	}
	if counted > 0 {
		rep.Totals.MeanCostUSD = rep.Totals.TotalCostUSD / float64(counted)
		rep.Totals.MeanDurationMS = totalDuration / int64(counted)
		rep.Totals.MeanToolCalls = float64(totalTools) / float64(counted)
	}
}

func printSummary(rep *report) {
	t := rep.Totals
	fmt.Printf("\n  planted defects   %d\n", t.PlantedDefects)
	fmt.Printf("  true positives    %d\n", t.TruePositives)
	fmt.Printf("  false positives   %d  (of which %d on the clean fixture)\n", t.FalsePositives, t.CleanFixtureFP)
	fmt.Printf("  false negatives   %d\n", t.FalseNegatives)
	fmt.Printf("  precision         %.3f\n", t.Precision)
	fmt.Printf("  recall            %.3f\n", t.Recall)
	fmt.Printf("  F1                %.3f\n", t.F1)
	fmt.Printf("  mean review       %.1fs, %.1f tool calls, $%.5f\n",
		float64(t.MeanDurationMS)/1000, t.MeanToolCalls, t.MeanCostUSD)

	for _, f := range rep.Fixtures {
		if len(f.Missed) == 0 && len(f.Spurious) == 0 {
			continue
		}
		fmt.Printf("\n  %s\n", f.Fixture)
		for _, m := range f.Missed {
			fmt.Printf("    MISSED    %s\n", m)
		}
		for _, s := range f.Spurious {
			fmt.Printf("    SPURIOUS  %s\n", s)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "evalrunner: "+format+"\n", args...)
	os.Exit(1)
}
