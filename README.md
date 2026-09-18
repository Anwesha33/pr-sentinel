# pr-sentinel

An asynchronous code-review service. You give it a GitHub pull request; it
clones the head revision, runs static analysis and the project's own tests,
lets an LLM investigate the change with real tools (`read_file`,
`search_code`, `run_tests`), validates every finding against the actual diff,
and posts a review back to GitHub.

Reviews run through Kafka rather than in the request, because a review takes
minutes and a GitHub webhook must be answered in milliseconds.

```
                 POST /v1/reviews
 GitHub webhook ────────────────▶ API ──▶ Postgres (job row, idempotency key)
                                   │
                                   └────▶ Kafka: pr.review.requested
                                                    │
                                                    ▼
                                            ┌───────────────┐
                                            │    worker     │
                                            └───────────────┘
                                                    │
     ┌──────────────────────────────────────────────┼───────────────────────┐
     ▼                    ▼                         ▼                       ▼
 GitHub API          git clone --depth 1      static analysis         agent loop
 (PR + diff)         (head revision)          (vet / lint / build)    (LLM + tools)
                                                                            │
                                     findings validated against the diff ◀──┘
                                                    │
                        ┌───────────────────────────┴──────────────────┐
                        ▼                                              ▼
                 POST /pulls/{n}/reviews                    Postgres: findings,
                 (inline comments)                          agent steps, usage

  failure ──▶ pr.review.requested.retry.{10s,60s,300s} ──▶ back to the main topic
          ──▶ pr.review.dlq once the ladder is exhausted
```

## What it demonstrates

- **An LLM agent with real tools** — a bounded tool-calling loop where the model
  reads files, greps the repository and runs the test suite before concluding
  anything, with every tool call persisted as an audit trail.
- **Asynchronous backend architecture** — Kafka with tiered retry topics,
  at-least-once delivery made effectively-once by a claim in Postgres, a
  dead-letter topic, and recovery of jobs whose worker died mid-run.
- **Guardrails, not vibes** — every finding must anchor to a line the pull
  request actually changed, or it is downgraded to the summary instead of being
  posted inline. Findings the model is unsure about are dropped and recorded
  with a reason.
- **Measured quality** — `make eval` scores the agent against fixtures with
  planted defects and reports precision, recall, cost and latency. A prompt
  change can be shown to help rather than argued about.

## Quick start

```bash
cp .env.example .env         # add GEMINI_API_KEY, and GITHUB_TOKEN to post
make up                      # postgres + redis + kafka + api + 2 workers
make submit PR=https://github.com/octocat/hello-world/pull/1
make status JOB=<uuid from the response>
make trace  JOB=<uuid>       # what the agent actually did, step by step
```

`DRY_RUN=true` is the default: the review is computed and stored but nothing is
posted to GitHub. Set `DRY_RUN=false` (or `{"dry_run": false}` on the request)
to publish.

Run it from source instead:

```bash
make infra                   # just postgres, redis, kafka
go run ./cmd/api &
GEMINI_API_KEY=... go run ./cmd/worker
```

## API

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/v1/reviews` | Queue a review. Body: `{"repo_url": "..."}` or `{"owner","repo","pr_number"}`. Returns `202` with a job id; a repeat of an in-flight request returns `200` and the same id. |
| `GET` | `/v1/reviews/{id}` | Job status, token usage, findings, and the posted review. |
| `GET` | `/v1/reviews/{id}/steps` | Every tool call the agent made, with arguments, latency and cache hits. |
| `GET` | `/v1/reviews?status=failed` | Recent jobs by state. |
| `POST` | `/webhooks/github` | `pull_request` events (`opened`, `reopened`, `synchronize`), HMAC-verified. |
| `GET` | `/healthz`, `/readyz` | Liveness, and readiness gated on Postgres. |

## The agent's tools

| Tool | What it is for | Limits |
| --- | --- | --- |
| `read_file` | See code the diff references but does not show | 400 lines per call, Redis-cached |
| `search_code` | Find callers, duplicates of a bug, existing helpers | RE2, 100 matches, falls back to literal search when the model writes PCRE |
| `list_files` | Orient before reading | 300 entries, skips `node_modules`, `vendor`, `.git` |
| `run_tests` | Confirm a suspicion against reality | once per review, 4-minute timeout, network off |
| `run_static_analysis` | Full diagnostics | twice per review |

Tool results are always bounded and tool failures are returned to the model as
data (`{"error": "cannot read X: ... nearby files: ..."}`) rather than
aborting the loop — the model recovers from a wrong path, but not from the loop
being torn down.

## Evaluation

`eval/fixtures/` holds small pull requests with known defects — an off-by-one
index, a leaked `*sql.Rows` plus a SQL injection, a data race on a shared map,
a discarded error leading to a nil dereference, a read-modify-write lost update
— plus one deliberately clean refactor that measures how often the reviewer
invents problems in correct code.

```bash
make eval        # writes eval/report.json
```

Results are in [docs/RESULTS.md](docs/RESULTS.md).

## Configuration

Everything is read from the environment; see `internal/config/config.go`.

| Variable | Default | Notes |
| --- | --- | --- |
| `GEMINI_API_KEY` | — | Required by the worker |
| `GEMINI_MODEL` | `gemini-flash-latest` | The alias is deliberate; pinned model ids get retired |
| `GITHUB_TOKEN` | — | Needed for private repositories and for posting |
| `GITHUB_WEBHOOK_SECRET` | — | When set, webhook signatures are enforced |
| `DRY_RUN` | `true` | Compute the review without publishing it |
| `MAX_AGENT_STEPS` | `12` | Tool-call budget per review |
| `MAX_DELIVERY_ATTEMPTS` | `4` | Attempts before the dead-letter topic |
| `WORKER_CONCURRENCY` | `4` | Consumers per worker process |
| `JOB_TIMEOUT` | `10m` | Hard ceiling on one review |

## Repository layout

```
cmd/api          HTTP surface: validate, persist, enqueue, return 202
cmd/worker       Kafka consumers, retry tiers, stale-job reclaimer
cmd/evalrunner   Scores the agent against the fixtures
internal/agent   The tool-calling loop, tool implementations, finding validation
internal/diff    Unified-diff parser and GitHub comment anchoring
internal/ghclient GitHub REST client: rate limits, diff media type, 422 fallback
internal/llm     Gemini transport: function calling, token accounting, retries
internal/analysis Toolchain detection and the allowlisted command runner
internal/store   Job state machine, findings, agent-step audit trail
internal/queue   Kafka producer/consumer, retry ladder, DLQ
internal/cache   Redis: caches, and the per-pull-request lock
eval/fixtures    Pull requests with planted defects
docs/            Architecture, results, and the interview guide
```

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — why the pieces are shaped this way
- [docs/RESULTS.md](docs/RESULTS.md) — measured precision, recall, cost, latency
- [docs/INTERVIEW-GUIDE.md](docs/INTERVIEW-GUIDE.md) — design decisions, trade-offs, and the bugs found while building it
