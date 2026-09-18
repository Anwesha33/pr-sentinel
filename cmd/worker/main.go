// Command worker consumes review requests from Kafka and runs the agent.
//
// Delivery semantics: at-least-once from Kafka, effectively-once in effect,
// because the claim in Postgres is what authorises the work and a claim can
// only be won once per attempt. A duplicate delivery therefore costs one
// database round trip, not a duplicate review on the pull request.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Anwesha33/pr-sentinel/internal/cache"
	"github.com/Anwesha33/pr-sentinel/internal/config"
	"github.com/Anwesha33/pr-sentinel/internal/ghclient"
	"github.com/Anwesha33/pr-sentinel/internal/llm"
	"github.com/Anwesha33/pr-sentinel/internal/queue"
	"github.com/Anwesha33/pr-sentinel/internal/reviewer"
	"github.com/Anwesha33/pr-sentinel/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration", "err", err)
		os.Exit(1)
	}
	if cfg.GeminiAPIKey == "" {
		log.Error("GEMINI_API_KEY is required for the worker")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openStoreWithRetry(ctx, cfg.PostgresDSN, log)
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}

	rdb := cache.New(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	if err := rdb.Ping(ctx); err != nil {
		// Redis is a performance dependency, not a correctness one: every cache
		// read falls through to the real source. Start without it rather than
		// refusing to review anything.
		log.Warn("redis unavailable; running without cache", "err", err)
		rdb = nil
	} else {
		defer rdb.Close()
	}

	gh := ghclient.New(cfg.GitHubAPIBase, cfg.GitHubToken)
	if cfg.GitHubToken != "" {
		if login, err := gh.CurrentUser(ctx); err != nil {
			log.Warn("github token check failed", "err", err)
		} else {
			log.Info("github authenticated", "login", login)
		}
	} else {
		log.Warn("no GITHUB_TOKEN set; only public repositories will be reviewable and posting is impossible")
	}

	producer := queue.NewProducer(cfg.KafkaBrokers)
	defer producer.Close()

	rv := &reviewer.Reviewer{
		Cfg:   cfg,
		GH:    gh,
		LLM:   llm.New(cfg.GeminiAPIKey, cfg.GeminiModel, cfg.LLMTimeout),
		Cache: rdb,
		Store: st,
		Log:   slogAdapter{log},
	}

	w := &worker{cfg: cfg, store: st, cache: rdb, producer: producer, reviewer: rv, log: log}

	var wg sync.WaitGroup

	// Main topic: one consumer per configured slot, all in the same group, so
	// Kafka assigns partitions between them.
	for i := 0; i < cfg.WorkerConcurrency; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w.consumeMain(ctx, n)
		}(i)
	}

	// Retry tiers: each has a dedicated consumer whose only job is to wait out
	// the tier delay and republish.
	for tier := 1; tier <= len(queue.RetryTiers); tier++ {
		topic, delay, _ := queue.RetryTopic(cfg.ReviewTopic, tier)
		wg.Add(1)
		go func(topic string, delay time.Duration) {
			defer wg.Done()
			w.consumeRetry(ctx, topic, delay)
		}(topic, delay)
	}

	// Reclaim jobs whose worker died mid-review.
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.reclaimLoop(ctx)
	}()

	log.Info("worker started", "concurrency", cfg.WorkerConcurrency, "topic", cfg.ReviewTopic,
		"model", cfg.GeminiModel, "dry_run_default", cfg.DryRun)

	<-ctx.Done()
	log.Info("shutdown signal received; finishing in-flight jobs")
	wg.Wait()
	log.Info("worker stopped")
}

type worker struct {
	cfg      *config.Config
	store    *store.Store
	cache    *cache.Cache
	producer *queue.Producer
	reviewer *reviewer.Reviewer
	log      *slog.Logger
}

func (w *worker) consumeMain(ctx context.Context, n int) {
	c := queue.NewConsumer(w.cfg.KafkaBrokers, w.cfg.ReviewTopic, w.cfg.ConsumerGroup)
	defer c.Close()

	for {
		msg, err := c.Fetch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.log.Error("fetch", "consumer", n, "err", err)
			time.Sleep(time.Second)
			continue
		}

		w.handle(ctx, msg.Request)

		// Commit after handling. The job state machine — not the offset — is
		// what prevents a duplicate review, so committing late is safe and
		// committing early would lose work on a crash.
		if err := c.Commit(ctx, msg); err != nil {
			w.log.Error("commit", "err", err, "job_id", msg.Request.JobID)
		}
	}
}

// consumeRetry implements the delay tier: hold the message until its delay has
// elapsed, then put it back on the main topic.
func (w *worker) consumeRetry(ctx context.Context, topic string, delay time.Duration) {
	c := queue.NewConsumer(w.cfg.KafkaBrokers, topic, w.cfg.ConsumerGroup+".retry")
	defer c.Close()

	for {
		msg, err := c.Fetch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(time.Second)
			continue
		}

		wait := time.Until(msg.Request.NotBefore)
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}

		req := msg.Request
		req.NotBefore = time.Time{}
		if err := w.producer.Publish(ctx, w.cfg.ReviewTopic, req); err != nil {
			w.log.Error("republish from retry tier", "topic", topic, "err", err)
			// Do not commit: let it be redelivered rather than dropped.
			continue
		}
		w.log.Info("retry released", "job_id", req.JobID, "attempt", req.Attempt, "tier", delay.String())
		if err := c.Commit(ctx, msg); err != nil {
			w.log.Error("commit retry", "err", err)
		}
	}
}

func (w *worker) handle(ctx context.Context, req queue.ReviewRequest) {
	log := w.log.With("job_id", req.JobID, "repo", req.RepoOwner+"/"+req.RepoName, "pr", req.PRNumber)

	job, err := w.store.ClaimJob(ctx, req.JobID)
	switch {
	case errors.Is(err, store.ErrAlreadyClaimed):
		log.Info("skipping duplicate delivery; job already claimed")
		return
	case errors.Is(err, store.ErrNotFound):
		log.Warn("message references an unknown job; dropping")
		return
	case err != nil:
		log.Error("claim job", "err", err)
		return
	}

	// A second guard against two workers reviewing one PR: the claim above
	// covers a repeat of the same job, this covers two different jobs racing on
	// the same pull request (webhook + manual submit).
	var lock *cache.Lock
	if w.cache != nil {
		lockKey := fmt.Sprintf("pr:%s/%s#%d", job.RepoOwner, job.RepoName, job.PRNumber)
		l, ok, lockErr := w.cache.AcquireLock(ctx, lockKey, w.cfg.JobTimeout)
		if lockErr != nil {
			log.Warn("lock unavailable; proceeding without it", "err", lockErr)
		} else if !ok {
			log.Info("another review of this pull request is in flight; deferring")
			w.scheduleRetry(ctx, job, req, "another review of this pull request is in flight")
			return
		} else {
			lock = l
			defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
		}
	}

	jobCtx, cancel := context.WithTimeout(ctx, w.cfg.JobTimeout)
	defer cancel()

	// Keep the lock alive for as long as the job runs.
	if lock != nil {
		go func() {
			t := time.NewTicker(w.cfg.JobTimeout / 3)
			defer t.Stop()
			for {
				select {
				case <-jobCtx.Done():
					return
				case <-t.C:
					_ = lock.Extend(jobCtx, w.cfg.JobTimeout)
				}
			}
		}()
	}

	start := time.Now()
	outcome, err := w.reviewer.Run(jobCtx, job)
	if err != nil {
		log.Error("review failed", "err", err, "attempt", job.Attempts)
		w.scheduleRetry(ctx, job, req, err.Error())
		return
	}

	if outcome.Skipped != "" {
		log.Info("review skipped", "reason", outcome.Skipped)
		_ = w.store.MarkSucceeded(ctx, job.ID, store.Usage{DurationMS: time.Since(start).Milliseconds()})
		return
	}

	all := append(append([]store.Finding{}, outcome.Result.Findings...), outcome.Result.Dropped...)
	if err := w.store.ReplaceFindings(ctx, job.ID, all); err != nil {
		log.Error("persist findings", "err", err)
	}
	usage := outcome.Result.Usage
	usage.DurationMS = time.Since(start).Milliseconds()
	if err := w.store.MarkSucceeded(ctx, job.ID, usage); err != nil {
		log.Error("mark succeeded", "err", err)
	}
	log.Info("review complete",
		"findings", len(outcome.Result.Findings), "dropped", len(outcome.Result.Dropped),
		"tool_calls", len(outcome.Result.Steps), "llm_calls", usage.LLMCalls,
		"prompt_tokens", usage.PromptTokens, "output_tokens", usage.OutputTokens,
		"duration_ms", usage.DurationMS, "dry_run", job.DryRun)
}

// scheduleRetry records the failure and either publishes to the next delay tier
// or routes to the dead-letter topic.
func (w *worker) scheduleRetry(ctx context.Context, job *store.Job, req queue.ReviewRequest, cause string) {
	willRetry, err := w.store.MarkFailure(ctx, job.ID, cause, store.Usage{})
	if err != nil {
		w.log.Error("mark failure", "err", err, "job_id", job.ID)
	}

	next := req
	next.Attempt = job.Attempts + 1
	next.LastError = cause

	if willRetry {
		topic, delay, ok := queue.RetryTopic(w.cfg.ReviewTopic, job.Attempts)
		if ok {
			next.NotBefore = time.Now().Add(delay)
			if pubErr := w.producer.Publish(ctx, topic, next); pubErr == nil {
				w.log.Info("scheduled retry", "job_id", job.ID, "topic", topic,
					"delay", delay.String(), "attempt", next.Attempt)
				return
			} else {
				w.log.Error("publish retry", "err", pubErr, "job_id", job.ID)
			}
		}
	}

	next.NotBefore = time.Time{}
	if err := w.producer.Publish(ctx, w.cfg.ReviewDLQTopic, next); err != nil {
		w.log.Error("publish to DLQ", "err", err, "job_id", job.ID)
		return
	}
	w.log.Warn("job dead-lettered", "job_id", job.ID, "attempts", job.Attempts, "cause", cause)
}

// reclaimLoop rescues jobs left in `running` by a worker that died.
func (w *worker) reclaimLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := w.store.ReclaimStale(ctx, 2*w.cfg.JobTimeout)
			if err != nil {
				w.log.Error("reclaim stale jobs", "err", err)
				continue
			}
			if n > 0 {
				w.log.Warn("reclaimed stale jobs", "count", n)
			}
		}
	}
}

type slogAdapter struct{ l *slog.Logger }

func (s slogAdapter) Info(msg string, kv ...any)  { s.l.Info(msg, kv...) }
func (s slogAdapter) Error(msg string, kv ...any) { s.l.Error(msg, kv...) }

func openStoreWithRetry(ctx context.Context, dsn string, log *slog.Logger) (*store.Store, error) {
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		st, err := store.Open(ctx, dsn)
		if err == nil {
			return st, nil
		}
		lastErr = err
		log.Info("waiting for postgres", "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, lastErr
}
