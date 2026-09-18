# Architecture

## The shape of the problem

A code review is a slow, expensive, failure-prone operation triggered by a
webhook that must be answered in milliseconds. Everything else follows from
that sentence.

Slow: cloning a repository, running a test suite and making several LLM calls
takes seconds to minutes. Expensive: every retry costs real money, so retries
must be deliberate. Failure-prone: GitHub rate-limits, the LLM returns 503 under
load, a test suite hangs, a worker is evicted mid-review. And the webhook needs
a 2xx immediately or GitHub eventually disables it.

So the API does no work. It validates, writes a job row, publishes to Kafka and
returns 202. A pool of workers does everything else.

## Job lifecycle

```
queued ──▶ running ──▶ succeeded
             │  │
             │  └──▶ retrying ──▶ (delay topic) ──▶ running
             └─────▶ failed ────▶ dead-letter topic
```

`running` is only ever entered by a worker that won this update:

```sql
UPDATE review_jobs SET status='running', attempts=attempts+1
WHERE id=$1 AND status IN ('queued','retrying')
RETURNING ...
```

That `WHERE` clause is the concurrency control for the whole system. Kafka
guarantees at-least-once delivery, so two workers can be handed the same
message; both race on this statement and exactly one gets a row back. The loser
sees `ErrAlreadyClaimed`, commits its offset and moves on. Delivery stays
at-least-once; *reviewing* becomes effectively-once.

The API adds a second layer in front: an idempotency key. Submitting the same
pull request twice returns the existing job rather than creating a second one,
and the webhook path puts the head SHA in the key so a re-delivered webhook
deduplicates while a genuine new push still gets its own review.

## Why retries live on their own topics

Kafka has no per-message visibility timeout. The naive way to retry in 30
seconds is to sleep before committing the offset — which stalls the entire
partition and blocks every healthy message queued behind the poisoned one.

Instead a failed job is republished to a delay topic whose consumer does
nothing but wait out that tier's fixed delay and put the message back:

```
pr.review.requested ──fail──▶ ...retry.10s ──▶ back to requested
                    ──fail──▶ ...retry.60s ──▶ back to requested
                    ──fail──▶ ...retry.300s ─▶ back to requested
                    ──exhausted──▶ pr.review.dlq
```

Head-of-line blocking still exists inside a retry topic, but it is bounded by
that tier's delay and only ever affects other failures. Healthy traffic never
queues behind a failure.

A worker that dies mid-review leaves its job stuck in `running`. A reclaimer
sweeps jobs whose lease has expired back to `retrying` once a minute, so a pod
eviction costs a delay rather than a lost review.

## Why Redis is here, given Postgres already guards correctness

Three jobs, none of them correctness:

- **Caching file reads and searches.** An agent re-reads the same file across
  steps; the cache makes that free.
- **A per-pull-request lock.** The Postgres claim stops one *job* being run
  twice. The lock stops two *different* jobs — a webhook and a manual submit —
  reviewing the same pull request at the same time and posting two reviews.
- **Lease extension.** A long review renews its lock while it runs.

The lock is plain `SET NX PX`, not Redlock. Losing it costs one duplicated
review, not a correctness violation, because Postgres remains the authority.
Paying for a consensus protocol to save a duplicate would be the wrong trade.
When Redis is down the worker logs a warning and runs without it.

## The agent loop

Two LLM phases, deliberately separated:

1. **Investigate**, tools enabled. The model reads files, greps the repository
   and can run the test suite. Its prose here is scratch work and is never
   shown to anyone.
2. **Extract**, tools disabled, `responseMimeType: application/json`. The model
   converts its own conclusions into structured findings.

They are split for two reasons. The API refuses to combine function calling
with forced JSON output. And mixing "think" with "format" in one call degraded
both — the model either rushed to the JSON without investigating, or produced
JSON with tool-call fragments inside it.

Every tool result is bounded, and every tool *failure* is returned to the model
as data rather than raised as an error:

```json
{"error": "cannot read internal/wallet.go: no such file. Nearby files: internal/wallet/wallet.go, ..."}
```

A model recovers from a wrong path immediately. It cannot recover from the loop
being torn down. Limits are enforced in code, not in the prompt: `run_tests` is
refused after the first call no matter how the model asks.

## The validation pass

This is where the project stops being a wrapper around a model.

Every finding must resolve to a line the pull request actually changed. The
diff parser builds, for each file, a map from `(side, line)` to a legal comment
anchor — both the modern `line`+`side` pair and the legacy `position` offset.
Then:

- **Exact match** on an added line: anchored inline.
- **Near miss** within three lines: snapped to the nearest added line. Models
  reason about the file and are routinely one or two lines off; dropping those
  findings was measurably worse than snapping them.
- **Far miss or unknown file**: kept, but marked unanchored and rendered in the
  summary body instead of inline, with the reason recorded.

This matters because GitHub rejects an *entire* review with 422 if even one
comment points outside the diff. One bad coordinate would otherwise lose every
good finding alongside it. The client also degrades on 422 by reposting without
inline comments, so a review is never lost to an anchoring bug.

Below the confidence threshold a finding is dropped outright — with its reason
stored, so the drop rate is a number rather than a mystery.

## Sandboxing

The agent can run commands, so the commands are a fixed allowlist per detected
toolchain (`go vet ./...`, `npm test`, `python3 -m pytest -q`, …). The model
chooses *an action*, never an argv. Each command gets a timeout, a scratch
`HOME` inside the workspace, package managers pinned offline, and a truncated
output buffer that keeps the head and the tail — compilers put the first error
at the top, test runners put the summary at the bottom.

Path arguments come from the model, so they are untrusted input. Traversal is
clamped into the workspace rather than rejected: `../../etc/passwd` resolves to
a path inside the workspace that does not exist, which produces a useful error
instead of a leaked host file.

The honest limit: this is an allowlist and a timeout, not a sandbox. Running a
pull request's test suite executes that pull request's code. A production
deployment would run each review in a disposable container with no network and
no credentials. The `Dockerfile` is the right place for that, and it is not
what a review bot gets for free.

## Observability

Every tool call is persisted: name, arguments, result size, latency, whether it
was a cache hit, and any error. `GET /v1/reviews/{id}/steps` replays exactly
what the agent did. Without it, "the review was strange" is unanswerable; with
it, the answer is usually "it read the wrong file at step 4".

Token counts, LLM call counts and wall-clock duration are stored per job, which
is what makes the cost-per-review figure in RESULTS.md a measurement rather
than an estimate.
