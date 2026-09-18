# Evaluation results

Reproduce with `make eval`, which writes `eval/report.json`.

## Method

Six fixture pull requests, each a small real change with defects planted at
known locations. A finding counts as a true positive when it names the right
file and lands within three lines of the planted defect; each defect can be
claimed by at most one finding, and every unclaimed finding is a false
positive. One fixture is a correct refactor with nothing wrong in it — every
finding it draws is a false positive, which is the number that decides whether
a team keeps the bot switched on or mutes it.

Line numbers in the fixtures are resolved from code anchors rather than
hard-coded, so editing a fixture cannot silently invalidate its expectations.

## Results

Model `gemini-3.5-flash-lite`, tool budget 4, confidence threshold 0.5,
line tolerance 3.

| Fixture | Planted | TP | FP | FN | Tool calls | Latency | Cost |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `01-offbyone-window` | 2 | 1 | 0 | 1 | 2 | 5.9s | $0.00098 |
| `02-rows-leak` | 2 | 2 | 0 | 0 | 1 | 4.9s | $0.00084 |
| `03-map-race` | 2 | 2 | 0 | 0 | 1 | 5.2s | $0.00083 |
| `04-nil-deref` | 1 | 1 | 0 | 0 | 1 | 4.2s | $0.00065 |
| `05-lost-update` | 1 | 1 | 0 | 0 | 2 | 70.5s | $0.00095 |
| `06-clean-refactor` | 0 | 0 | 0 | 0 | 4 | 7.7s | $0.00127 |

| Metric | Value |
| --- | --- |
| Planted defects | 8 |
| True positives | 7 |
| False positives | 0 |
| False negatives | 1 |
| **Precision** | **1.000** |
| **Recall** | **0.875** |
| **F1** | **0.933** |
| False positives on the clean fixture | 0 |
| Mean latency per review | 16.4s |
| Mean tool calls per review | 1.8 |
| Mean cost per review | $0.00092 |

## Reading these numbers honestly

**Precision of 1.000 is the headline, and it is the number that took the work.**
Zero false positives across five defective pull requests *and* a clean one is
not what an unconstrained model does. Earlier runs produced confident findings
about code the diff did not touch and duplicate reports of the same defect at
two different lines. What removed them was not a better prompt but the
validation pass: a finding must anchor to a line the pull request changed, must
clear a confidence threshold, and is deduplicated on file+line+title. Findings
that fail anchoring are not deleted, they are downgraded into the summary and
recorded with a reason, so the failure mode stays measurable instead of
invisible.

**The one miss is instructive.** In `01-offbyone-window` the reviewer caught
that `Percentile(100)` indexes one past the end, but not that the same function
panics on an empty window — even though the function directly above it has
exactly that guard. It found the defect it was looking for and stopped. That is
the characteristic failure of a budgeted agent: once it has a good answer, it
stops investigating. Raising the tool budget is the obvious lever, and the cost
column is why it is not free.

**Latency is bimodal, and the mean hides it.** Five reviews finished in 4–8
seconds; `05-lost-update` took 70 seconds because the model chose to run the
test suite. The p50 here is about 6 seconds and the p95 is the test-running
case. That is the right shape — reviews that need evidence should pay for it —
but it means "mean latency" is close to meaningless for capacity planning.

**Cost is not the constraint people expect.** At roughly $0.0009 a review, a
repository merging 200 pull requests a month costs about 18 cents. The real
constraints are the API's rate limits and the wall-clock cost of cloning and
testing, not the token bill.

## What these numbers do not show

- **Six fixtures is a small sample.** Precision of 1.000 over 7 findings means
  "no false positives yet", not "never produces one". The honest claim is that
  the guardrails are working on the failure modes they were built for.
- **The fixtures are small and single-file.** Real pull requests span many
  files and carry context the diff does not show. The tool loop exists for
  exactly that case, and it is the case these fixtures test least.
- **Defects were planted deliberately.** They are realistic — every one is a
  bug pattern that reaches production regularly — but a planted defect is
  cleaner than one that arises from a misunderstanding between two engineers.
- **One model, one run.** There is no variance measurement here. Temperature is
  0.1 during investigation and 0 during extraction, which makes runs fairly
  stable, but "fairly stable" is not "measured".

## A note on the model used

The service defaults to `gemini-flash-latest`. These results were measured on
`gemini-3.5-flash-lite` because the Gemini free tier allows 20 requests per day
*per model*, and a full six-fixture run needs roughly 20 calls — the stronger
models' daily budgets were spent during development. On a billed key, re-run
with:

```bash
GEMINI_MODEL=gemini-flash-latest make eval
```

The scoring harness records the model in `eval/report.json`, so results from
different models are never silently mixed.
