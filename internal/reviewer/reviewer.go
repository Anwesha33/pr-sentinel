// Package reviewer is the end-to-end pipeline for one review job: fetch the
// pull request, materialise its head revision, run the deterministic checks,
// run the agent, then publish the result.
//
// It is separated from the Kafka worker so the same pipeline can be driven by
// the evaluation harness with no broker, no database and no network.
package reviewer

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Anwesha33/pr-sentinel/internal/agent"
	"github.com/Anwesha33/pr-sentinel/internal/analysis"
	"github.com/Anwesha33/pr-sentinel/internal/cache"
	"github.com/Anwesha33/pr-sentinel/internal/config"
	"github.com/Anwesha33/pr-sentinel/internal/diff"
	"github.com/Anwesha33/pr-sentinel/internal/ghclient"
	"github.com/Anwesha33/pr-sentinel/internal/llm"
	"github.com/Anwesha33/pr-sentinel/internal/store"
	"github.com/Anwesha33/pr-sentinel/internal/workspace"
)

type Reviewer struct {
	Cfg   *config.Config
	GH    *ghclient.Client
	LLM   *llm.Client
	Cache *cache.Cache
	Store *store.Store
	Log   Logger
}

// Logger is the minimal logging surface the pipeline needs, so the package does
// not force a logging library on its callers.
type Logger interface {
	Info(msg string, kv ...any)
	Error(msg string, kv ...any)
}

// Outcome is what the worker persists.
type Outcome struct {
	Result  *agent.Result
	HeadSHA string
	Posted  *store.PostedReview
	Skipped string
}

// Run executes the whole pipeline for one job.
func (r *Reviewer) Run(ctx context.Context, job *store.Job) (*Outcome, error) {
	owner, repo, number := job.RepoOwner, job.RepoName, job.PRNumber

	pr, err := r.GH.GetPullRequest(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("fetch pull request: %w", err)
	}
	if pr.State != "open" {
		// Reviewing a merged or closed PR wastes an LLM budget on a decision
		// nobody can act on.
		return &Outcome{Skipped: fmt.Sprintf("pull request is %s, not open", pr.State), HeadSHA: pr.Head.SHA}, nil
	}
	if r.Store != nil {
		_ = r.Store.SetHeadSHA(ctx, job.ID, pr.Head.SHA)
	}
	r.Log.Info("fetched pull request", "repo", owner+"/"+repo, "pr", number,
		"head", pr.Head.SHA[:min(7, len(pr.Head.SHA))], "files", pr.ChangedFiles)

	rawDiff, err := r.GH.GetPullRequestDiff(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("fetch diff: %w", err)
	}
	files, err := diff.Parse(rawDiff)
	if err != nil {
		return nil, fmt.Errorf("parse diff: %w", err)
	}
	if len(files) == 0 {
		return &Outcome{Skipped: "diff is empty", HeadSHA: pr.Head.SHA}, nil
	}

	ws, err := workspace.Clone(ctx, r.Cfg.WorkspaceRoot, owner, repo, pr.Head.SHA, r.Cfg.GitHubToken)
	if err != nil {
		return nil, fmt.Errorf("clone workspace: %w", err)
	}
	defer func() {
		if cErr := ws.Close(); cErr != nil {
			r.Log.Error("workspace cleanup failed", "err", cErr)
		}
	}()

	runner := analysis.NewRunner(ws.Root)
	toolchain := analysis.Detect(ws.Root)
	staticResults := runner.StaticAnalysis(ctx)
	staticReport := analysis.Summarize(staticResults)
	r.Log.Info("static analysis complete", "toolchain", string(toolchain), "checks", len(staticResults))

	ag := &agent.Agent{
		LLM: r.LLM,
		Tools: &agent.Toolbox{
			WS: ws, Runner: runner, Cache: r.Cache, MaxFileBytes: r.Cfg.MaxFileReadBytes,
		},
		MaxSteps:      r.Cfg.MaxAgentSteps,
		LineTolerance: 3,
		MinConfidence: 0.5,
		Attempt:       job.Attempts,
		OnStep: func(st store.Step) {
			if r.Store == nil {
				return
			}
			// Persisted best-effort: losing an audit row must never fail a review.
			if err := r.Store.RecordStep(ctx, job.ID, st); err != nil {
				r.Log.Error("record agent step", "err", err)
			}
		},
	}

	result, err := ag.Review(ctx, agent.Input{
		Owner: owner, Repo: repo, PRNumber: number,
		Title: pr.Title, Description: pr.Body,
		HeadSHA: pr.Head.SHA, BaseRef: pr.Base.Ref,
		Files:        files,
		RenderedDiff: diff.Render(files, r.Cfg.MaxDiffBytes),
		StaticReport: staticReport,
		Toolchain:    toolchain,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	r.Log.Info("agent finished", "findings", len(result.Findings), "dropped", len(result.Dropped),
		"llm_calls", result.Usage.LLMCalls, "tool_calls", len(result.Steps))

	posted, err := r.publish(ctx, job, pr, result)
	if err != nil {
		return nil, err
	}
	return &Outcome{Result: result, HeadSHA: pr.Head.SHA, Posted: posted}, nil
}

func (r *Reviewer) publish(ctx context.Context, job *store.Job, pr *ghclient.PullRequest, res *agent.Result) (*store.PostedReview, error) {
	body := agent.RenderReviewBody(res, r.LLM.Model(), job.DryRun)

	var comments []ghclient.ReviewComment
	for _, f := range res.Findings {
		if !f.Anchored {
			continue // already folded into the summary body
		}
		comments = append(comments, ghclient.ReviewComment{
			Path: f.FilePath,
			Line: f.Line,
			Side: f.Side,
			Body: agent.RenderInlineComment(f),
		})
	}

	event := "COMMENT"
	// REQUEST_CHANGES is reserved for critical findings. A bot that requests
	// changes over a minor nit gets muted by the team within a week.
	for _, f := range res.Findings {
		if f.Severity == "critical" {
			event = "REQUEST_CHANGES"
			break
		}
	}
	// GitHub refuses APPROVE and REQUEST_CHANGES on your own pull request, and
	// the token here is often the author's. COMMENT always works.
	if event == "REQUEST_CHANGES" && r.Cfg.DryRun {
		event = "COMMENT"
	}

	rec := store.PostedReview{
		CommentCount: len(comments),
		Event:        event,
		DryRun:       job.DryRun,
		Body:         body,
		PostedAt:     time.Now(),
	}

	if job.DryRun {
		r.Log.Info("dry run: review computed but not published", "comments", len(comments), "event", event)
		return &rec, r.save(ctx, job.ID, rec)
	}

	review, err := r.GH.CreateReview(ctx, job.RepoOwner, job.RepoName, job.PRNumber, ghclient.CreateReviewInput{
		CommitID: pr.Head.SHA,
		Body:     body,
		Event:    event,
		Comments: comments,
	})
	if err != nil {
		return nil, fmt.Errorf("post review: %w", err)
	}
	rec.GitHubReviewID = review.ID
	rec.HTMLURL = review.HTMLURL
	r.Log.Info("review posted", "url", review.HTMLURL, "comments", len(comments))
	return &rec, r.save(ctx, job.ID, rec)
}

func (r *Reviewer) save(ctx context.Context, jobID uuid.UUID, rec store.PostedReview) error {
	if r.Store == nil {
		return nil
	}
	return r.Store.SavePostedReview(ctx, jobID, rec)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
