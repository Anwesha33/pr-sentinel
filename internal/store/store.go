// Package store is the Postgres persistence layer: the job state machine, the
// findings a review produced, and the agent's tool-call audit trail.
package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// Status values form a deliberately small state machine:
//
//	queued ──▶ running ──▶ succeeded
//	             │   │
//	             │   └──▶ retrying ──▶ running   (transient failure, attempts left)
//	             └──────▶ failed                 (attempts exhausted → DLQ)
//
// `running` is only ever set by a worker that won the claim, so a job can never
// be processed by two workers at once even though Kafka may deliver it twice.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusRetrying  = "retrying"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// ErrNotFound is returned when a job id does not exist.
var ErrNotFound = errors.New("not found")

// ErrAlreadyClaimed means another worker is already running this job, which is
// the normal outcome of a duplicate Kafka delivery — not an error worth a retry.
var ErrAlreadyClaimed = errors.New("job already claimed")

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, schemaSQL)
	return err
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Job is one review request.
type Job struct {
	ID             uuid.UUID  `json:"id"`
	IdempotencyKey string     `json:"idempotency_key"`
	RepoOwner      string     `json:"repo_owner"`
	RepoName       string     `json:"repo_name"`
	PRNumber       int        `json:"pr_number"`
	HeadSHA        string     `json:"head_sha"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	DryRun         bool       `json:"dry_run"`
	LastError      string     `json:"last_error,omitempty"`
	PromptTokens   int        `json:"prompt_tokens"`
	OutputTokens   int        `json:"output_tokens"`
	LLMCalls       int        `json:"llm_calls"`
	DurationMS     int64      `json:"duration_ms"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

const jobCols = `id, idempotency_key, repo_owner, repo_name, pr_number, head_sha, status,
	attempts, max_attempts, dry_run, last_error, prompt_tokens, output_tokens, llm_calls,
	duration_ms, created_at, updated_at, started_at, finished_at`

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.IdempotencyKey, &j.RepoOwner, &j.RepoName, &j.PRNumber,
		&j.HeadSHA, &j.Status, &j.Attempts, &j.MaxAttempts, &j.DryRun, &j.LastError,
		&j.PromptTokens, &j.OutputTokens, &j.LLMCalls, &j.DurationMS,
		&j.CreatedAt, &j.UpdatedAt, &j.StartedAt, &j.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// CreateJob inserts a queued job. When idempotencyKey already exists the
// existing job is returned with created=false, so a retried API call or a
// replayed webhook never produces a second review of the same commit.
func (s *Store) CreateJob(ctx context.Context, j *Job) (out *Job, created bool, err error) {
	if j.ID == uuid.Nil {
		j.ID = uuid.New()
	}
	if j.MaxAttempts == 0 {
		j.MaxAttempts = 4
	}
	const q = `INSERT INTO review_jobs (id, idempotency_key, repo_owner, repo_name, pr_number, head_sha, status, max_attempts, dry_run)
	           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	           ON CONFLICT (idempotency_key) DO NOTHING
	           RETURNING ` + jobCols
	row := s.pool.QueryRow(ctx, q, j.ID, j.IdempotencyKey, j.RepoOwner, j.RepoName,
		j.PRNumber, j.HeadSHA, StatusQueued, j.MaxAttempts, j.DryRun)
	created_, err := scanJob(row)
	if err == nil {
		return created_, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	existing, err := s.JobByIdempotencyKey(ctx, j.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

func (s *Store) JobByID(ctx context.Context, id uuid.UUID) (*Job, error) {
	return scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM review_jobs WHERE id = $1`, id))
}

func (s *Store) JobByIdempotencyKey(ctx context.Context, key string) (*Job, error) {
	return scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM review_jobs WHERE idempotency_key = $1`, key))
}

// ClaimJob moves a job into `running` and bumps its attempt counter, but only
// from a state where that is legal. The WHERE clause is the concurrency
// control: two workers handed the same Kafka message race here and exactly one
// wins; the loser gets ErrAlreadyClaimed and acknowledges its copy.
func (s *Store) ClaimJob(ctx context.Context, id uuid.UUID) (*Job, error) {
	const q = `UPDATE review_jobs
	           SET status = $2, attempts = attempts + 1, started_at = now(), updated_at = now()
	           WHERE id = $1 AND status IN ($3, $4)
	           RETURNING ` + jobCols
	j, err := scanJob(s.pool.QueryRow(ctx, q, id, StatusRunning, StatusQueued, StatusRetrying))
	if errors.Is(err, ErrNotFound) {
		// Either the id is unknown or the job is not claimable. Distinguish,
		// because "unknown id" is a bug and "already claimed" is routine.
		if _, e2 := s.JobByID(ctx, id); e2 == nil {
			return nil, ErrAlreadyClaimed
		}
		return nil, ErrNotFound
	}
	return j, err
}

// Usage carries the LLM accounting for one job run.
type Usage struct {
	PromptTokens int
	OutputTokens int
	LLMCalls     int
	DurationMS   int64
}

func (s *Store) MarkSucceeded(ctx context.Context, id uuid.UUID, u Usage) error {
	const q = `UPDATE review_jobs SET status=$2, last_error='', finished_at=now(), updated_at=now(),
	           prompt_tokens=$3, output_tokens=$4, llm_calls=$5, duration_ms=$6 WHERE id=$1`
	_, err := s.pool.Exec(ctx, q, id, StatusSucceeded, u.PromptTokens, u.OutputTokens, u.LLMCalls, u.DurationMS)
	return err
}

// MarkFailure records an error and decides the next state. It returns
// willRetry=true when attempts remain, which the worker uses to choose between
// re-publishing with backoff and routing to the dead-letter topic.
func (s *Store) MarkFailure(ctx context.Context, id uuid.UUID, cause string, u Usage) (willRetry bool, err error) {
	const q = `UPDATE review_jobs
	           SET status = CASE WHEN attempts < max_attempts THEN $2 ELSE $3 END,
	               last_error = $4,
	               finished_at = CASE WHEN attempts < max_attempts THEN NULL ELSE now() END,
	               updated_at = now(),
	               prompt_tokens = prompt_tokens + $5,
	               output_tokens = output_tokens + $6,
	               llm_calls = llm_calls + $7,
	               duration_ms = $8
	           WHERE id = $1
	           RETURNING status`
	var status string
	err = s.pool.QueryRow(ctx, q, id, StatusRetrying, StatusFailed, truncate(cause, 4000),
		u.PromptTokens, u.OutputTokens, u.LLMCalls, u.DurationMS).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return status == StatusRetrying, err
}

// ReclaimStale rescues jobs whose worker died mid-run: a job stuck in `running`
// past the lease window is pushed back to `retrying` so it can be picked up
// again. Without this a pod OOM silently strands the job forever.
func (s *Store) ReclaimStale(ctx context.Context, olderThan time.Duration) (int64, error) {
	const q = `UPDATE review_jobs
	           SET status = CASE WHEN attempts < max_attempts THEN $1 ELSE $2 END,
	               last_error = 'worker lease expired',
	               updated_at = now()
	           WHERE status = $3 AND started_at < now() - $4::interval`
	tag, err := s.pool.Exec(ctx, q, StatusRetrying, StatusFailed, StatusRunning,
		fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) SetHeadSHA(ctx context.Context, id uuid.UUID, sha string) error {
	_, err := s.pool.Exec(ctx, `UPDATE review_jobs SET head_sha=$2, updated_at=now() WHERE id=$1`, id, sha)
	return err
}

func (s *Store) ListJobs(ctx context.Context, status string, limit int) ([]*Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + jobCols + ` FROM review_jobs`
	args := []any{}
	if status != "" {
		q += ` WHERE status = $1`
		args = append(args, status)
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT %d`, limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Finding is one reviewable issue the agent decided to report.
type Finding struct {
	ID            int64   `json:"id,omitempty"`
	FilePath      string  `json:"file"`
	Line          int     `json:"line"`
	Side          string  `json:"side"`
	Position      int     `json:"position,omitempty"`
	Severity      string  `json:"severity"`
	Category      string  `json:"category"`
	Title         string  `json:"title"`
	Body          string  `json:"body"`
	Confidence    float64 `json:"confidence"`
	Anchored      bool    `json:"anchored"`
	DroppedReason string  `json:"dropped_reason,omitempty"`
}

// ReplaceFindings makes a job's findings exactly the given set. A retry of a
// job must not append a second copy of every finding, so the delete and the
// insert share one transaction.
func (s *Store) ReplaceFindings(ctx context.Context, jobID uuid.UUID, fs []Finding) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM findings WHERE job_id = $1`, jobID); err != nil {
		return err
	}
	for _, f := range fs {
		const q = `INSERT INTO findings (job_id, file_path, line, side, position, severity, category, title, body, confidence, anchored, dropped_reason)
		           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
		if _, err := tx.Exec(ctx, q, jobID, f.FilePath, f.Line, f.Side, f.Position, f.Severity,
			f.Category, f.Title, f.Body, f.Confidence, f.Anchored, f.DroppedReason); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) FindingsByJob(ctx context.Context, jobID uuid.UUID) ([]Finding, error) {
	const q = `SELECT id, file_path, line, side, position, severity, category, title, body, confidence, anchored, dropped_reason
	           FROM findings WHERE job_id = $1 ORDER BY file_path, line, id`
	rows, err := s.pool.Query(ctx, q, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Finding{}
	for rows.Next() {
		var f Finding
		if err := rows.Scan(&f.ID, &f.FilePath, &f.Line, &f.Side, &f.Position, &f.Severity,
			&f.Category, &f.Title, &f.Body, &f.Confidence, &f.Anchored, &f.DroppedReason); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Step is one tool invocation inside the agent loop.
type Step struct {
	Attempt    int            `json:"attempt"`
	StepNo     int            `json:"step"`
	ToolName   string         `json:"tool"`
	ToolArgs   map[string]any `json:"args"`
	ResultSize int            `json:"result_size"`
	ResultHead string         `json:"result_head"`
	Error      string         `json:"error,omitempty"`
	LatencyMS  int64          `json:"latency_ms"`
	CacheHit   bool           `json:"cache_hit"`
}

func (s *Store) RecordStep(ctx context.Context, jobID uuid.UUID, st Step) error {
	args, err := json.Marshal(st.ToolArgs)
	if err != nil {
		args = []byte(`{}`)
	}
	const q = `INSERT INTO agent_steps (job_id, attempt, step_no, tool_name, tool_args, result_size, result_head, error, latency_ms, cache_hit)
	           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	_, err = s.pool.Exec(ctx, q, jobID, st.Attempt, st.StepNo, st.ToolName, args,
		st.ResultSize, truncate(st.ResultHead, 2000), truncate(st.Error, 1000), st.LatencyMS, st.CacheHit)
	return err
}

func (s *Store) StepsByJob(ctx context.Context, jobID uuid.UUID) ([]Step, error) {
	const q = `SELECT attempt, step_no, tool_name, tool_args, result_size, result_head, error, latency_ms, cache_hit
	           FROM agent_steps WHERE job_id = $1 ORDER BY attempt, step_no`
	rows, err := s.pool.Query(ctx, q, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Step{}
	for rows.Next() {
		var st Step
		var raw []byte
		if err := rows.Scan(&st.Attempt, &st.StepNo, &st.ToolName, &raw, &st.ResultSize,
			&st.ResultHead, &st.Error, &st.LatencyMS, &st.CacheHit); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &st.ToolArgs)
		out = append(out, st)
	}
	return out, rows.Err()
}

// PostedReview records what was sent to GitHub (or what would have been sent,
// when dry_run is on).
type PostedReview struct {
	GitHubReviewID int64     `json:"github_review_id"`
	HTMLURL        string    `json:"html_url"`
	CommentCount   int       `json:"comment_count"`
	Event          string    `json:"event"`
	DryRun         bool      `json:"dry_run"`
	Body           string    `json:"body"`
	PostedAt       time.Time `json:"posted_at"`
}

func (s *Store) SavePostedReview(ctx context.Context, jobID uuid.UUID, r PostedReview) error {
	const q = `INSERT INTO posted_reviews (job_id, github_review_id, html_url, comment_count, event, dry_run, body)
	           VALUES ($1,$2,$3,$4,$5,$6,$7)
	           ON CONFLICT (job_id) DO UPDATE SET github_review_id=EXCLUDED.github_review_id,
	             html_url=EXCLUDED.html_url, comment_count=EXCLUDED.comment_count,
	             event=EXCLUDED.event, dry_run=EXCLUDED.dry_run, body=EXCLUDED.body, posted_at=now()`
	_, err := s.pool.Exec(ctx, q, jobID, r.GitHubReviewID, r.HTMLURL, r.CommentCount, r.Event, r.DryRun, r.Body)
	return err
}

func (s *Store) PostedReviewByJob(ctx context.Context, jobID uuid.UUID) (*PostedReview, error) {
	const q = `SELECT github_review_id, html_url, comment_count, event, dry_run, body, posted_at
	           FROM posted_reviews WHERE job_id = $1`
	var r PostedReview
	err := s.pool.QueryRow(ctx, q, jobID).Scan(&r.GitHubReviewID, &r.HTMLURL, &r.CommentCount,
		&r.Event, &r.DryRun, &r.Body, &r.PostedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &r, err
}

// IsUniqueViolation reports whether err is a Postgres duplicate-key error.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
