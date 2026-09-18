package queue

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRetryTopicLadder(t *testing.T) {
	want := []struct {
		attempt int
		topic   string
		delay   time.Duration
	}{
		{1, "pr.review.requested.retry.10s", 10 * time.Second},
		{2, "pr.review.requested.retry.60s", 60 * time.Second},
		{3, "pr.review.requested.retry.300s", 300 * time.Second},
	}
	for _, w := range want {
		topic, delay, ok := RetryTopic("pr.review.requested", w.attempt)
		if !ok || topic != w.topic || delay != w.delay {
			t.Fatalf("attempt %d -> (%q, %v, %v), want (%q, %v, true)",
				w.attempt, topic, delay, ok, w.topic, w.delay)
		}
	}
	// Past the end of the ladder the caller must dead-letter instead.
	if _, _, ok := RetryTopic("pr.review.requested", len(RetryTiers)+1); ok {
		t.Fatal("ladder must report exhaustion past the last tier")
	}
	if _, _, ok := RetryTopic("pr.review.requested", 0); ok {
		t.Fatal("attempt 0 is not a valid tier")
	}
}

func TestAllTopicsCoversEveryTier(t *testing.T) {
	topics := AllTopics("base", "base.dlq")
	if len(topics) != 2+len(RetryTiers) {
		t.Fatalf("got %d topics %v, want %d", len(topics), topics, 2+len(RetryTiers))
	}
	seen := map[string]bool{}
	for _, tp := range topics {
		if seen[tp] {
			t.Fatalf("duplicate topic %q", tp)
		}
		seen[tp] = true
	}
	if !seen["base"] || !seen["base.dlq"] {
		t.Fatalf("base and dlq topics must be present: %v", topics)
	}
}

// Messages about one pull request must share a partition key so that a
// re-review triggered by a new push cannot overtake the first one.
func TestPartitionKeyIsStablePerPullRequest(t *testing.T) {
	a := ReviewRequest{JobID: uuid.New(), RepoOwner: "o", RepoName: "r", PRNumber: 5}
	b := ReviewRequest{JobID: uuid.New(), RepoOwner: "o", RepoName: "r", PRNumber: 5, Attempt: 3}
	c := ReviewRequest{JobID: uuid.New(), RepoOwner: "o", RepoName: "r", PRNumber: 6}
	if PartitionKey(a) != PartitionKey(b) {
		t.Fatal("different attempts on one PR must share a partition key")
	}
	if PartitionKey(a) == PartitionKey(c) {
		t.Fatal("different PRs must not be forced onto one key")
	}
}
