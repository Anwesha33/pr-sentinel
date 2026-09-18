-- pr-sentinel schema.
--
-- Applied idempotently at process start (see store.Migrate). Kept as one file
-- because the project is small enough that a migration tool would be more
-- ceremony than value; every statement is written to be re-runnable.

CREATE TABLE IF NOT EXISTS review_jobs (
    id               UUID PRIMARY KEY,
    -- Kafka is at-least-once, and GitHub will happily redeliver a webhook.
    -- This key is what makes the whole pipeline effectively-once.
    idempotency_key  TEXT        NOT NULL UNIQUE,
    repo_owner       TEXT        NOT NULL,
    repo_name        TEXT        NOT NULL,
    pr_number        INTEGER     NOT NULL,
    head_sha         TEXT        NOT NULL DEFAULT '',
    status           TEXT        NOT NULL,
    attempts         INTEGER     NOT NULL DEFAULT 0,
    max_attempts     INTEGER     NOT NULL DEFAULT 4,
    dry_run          BOOLEAN     NOT NULL DEFAULT TRUE,
    last_error       TEXT        NOT NULL DEFAULT '',
    prompt_tokens    INTEGER     NOT NULL DEFAULT 0,
    output_tokens    INTEGER     NOT NULL DEFAULT 0,
    llm_calls        INTEGER     NOT NULL DEFAULT 0,
    duration_ms      BIGINT      NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at       TIMESTAMPTZ,
    finished_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS review_jobs_status_idx  ON review_jobs (status, created_at DESC);
CREATE INDEX IF NOT EXISTS review_jobs_pr_idx      ON review_jobs (repo_owner, repo_name, pr_number, created_at DESC);

CREATE TABLE IF NOT EXISTS findings (
    id            BIGSERIAL PRIMARY KEY,
    job_id        UUID        NOT NULL REFERENCES review_jobs(id) ON DELETE CASCADE,
    file_path     TEXT        NOT NULL,
    line          INTEGER     NOT NULL DEFAULT 0,
    side          TEXT        NOT NULL DEFAULT 'RIGHT',
    position      INTEGER     NOT NULL DEFAULT 0,
    severity      TEXT        NOT NULL,
    category      TEXT        NOT NULL,
    title         TEXT        NOT NULL,
    body          TEXT        NOT NULL,
    confidence    REAL        NOT NULL DEFAULT 0,
    -- anchored=false means the finding survived review but could not be tied to
    -- a line in the diff, so it is reported in the summary instead of inline.
    anchored      BOOLEAN     NOT NULL DEFAULT FALSE,
    dropped_reason TEXT       NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS findings_job_idx ON findings (job_id);

-- Every tool call the agent makes is recorded. This is the difference between
-- "the agent said something odd" and "the agent read the wrong file at step 4".
CREATE TABLE IF NOT EXISTS agent_steps (
    id          BIGSERIAL PRIMARY KEY,
    job_id      UUID        NOT NULL REFERENCES review_jobs(id) ON DELETE CASCADE,
    attempt     INTEGER     NOT NULL DEFAULT 1,
    step_no     INTEGER     NOT NULL,
    tool_name   TEXT        NOT NULL,
    tool_args   JSONB       NOT NULL DEFAULT '{}'::jsonb,
    result_size INTEGER     NOT NULL DEFAULT 0,
    result_head TEXT        NOT NULL DEFAULT '',
    error       TEXT        NOT NULL DEFAULT '',
    latency_ms  BIGINT      NOT NULL DEFAULT 0,
    cache_hit   BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS agent_steps_job_idx ON agent_steps (job_id, attempt, step_no);

CREATE TABLE IF NOT EXISTS posted_reviews (
    job_id           UUID        PRIMARY KEY REFERENCES review_jobs(id) ON DELETE CASCADE,
    github_review_id BIGINT      NOT NULL DEFAULT 0,
    html_url         TEXT        NOT NULL DEFAULT '',
    comment_count    INTEGER     NOT NULL DEFAULT 0,
    event            TEXT        NOT NULL DEFAULT 'COMMENT',
    dry_run          BOOLEAN     NOT NULL DEFAULT TRUE,
    body             TEXT        NOT NULL DEFAULT '',
    posted_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
