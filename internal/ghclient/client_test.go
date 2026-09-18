package ghclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "test-token")
	// Keep the backoff ladder from making the test slow.
	c.httpClient = &http.Client{Timeout: 5 * time.Second}
	return c
}

func TestGetPullRequestDiffUsesDiffMediaType(t *testing.T) {
	const body = "diff --git a/x b/x\n"
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.github.v3.diff" {
			t.Errorf("Accept = %q, want the diff media type", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(body))
	})
	out, err := c.GetPullRequestDiff(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out != body {
		t.Fatalf("diff = %q", out)
	}
}

// The single most important behaviour in this client: GitHub rejects an entire
// review with 422 when one comment is off the diff, so we must retry without
// the inline comments rather than losing the review.
func TestCreateReviewFallsBackToSummaryOn422(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var in CreateReviewInput
		_ = json.NewDecoder(r.Body).Decode(&in)

		if n == 1 {
			if len(in.Comments) == 0 {
				t.Error("first attempt should carry the inline comments")
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"PullRequestReviewComment","field":"line","code":"invalid","message":"line must be part of the diff"}]}`))
			return
		}
		if len(in.Comments) != 0 {
			t.Error("fallback must not resend inline comments")
		}
		if !strings.Contains(in.Body, "risky thing") {
			t.Errorf("fallback body must inline the findings, got %q", in.Body)
		}
		_ = json.NewEncoder(w).Encode(Review{ID: 99, HTMLURL: "https://example/review/99"})
	})

	review, err := c.CreateReview(context.Background(), "o", "r", 1, CreateReviewInput{
		Body:  "summary",
		Event: "COMMENT",
		Comments: []ReviewComment{
			{Path: "a.go", Line: 42, Side: "RIGHT", Body: "risky thing"},
		},
	})
	if err != nil {
		t.Fatalf("fallback should succeed, got %v", err)
	}
	if review.ID != 99 {
		t.Fatalf("review = %+v", review)
	}
	if calls.Load() != 2 {
		t.Fatalf("want exactly 2 calls (reject then fallback), got %d", calls.Load())
	}
}

func TestCreateReviewDoesNotRetryWhenThereAreNoComments(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Validation Failed"}`))
	})
	if _, err := c.CreateReview(context.Background(), "o", "r", 1, CreateReviewInput{Body: "b", Event: "COMMENT"}); err == nil {
		t.Fatal("a 422 with no comments to drop has no fallback and must surface")
	}
	if calls.Load() != 1 {
		t.Fatalf("must not retry a comment-less 422, got %d calls", calls.Load())
	}
}

func TestSecondaryRateLimitIsHonoured(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(PullRequest{Number: 7, Title: "ok"})
	})

	start := time.Now()
	pr, err := c.GetPullRequest(context.Background(), "o", "r", 7)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if pr.Number != 7 {
		t.Fatalf("pr = %+v", pr)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("Retry-After was ignored; retried after only %s", elapsed)
	}
}

func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&APIError{StatusCode: 422, Message: "Validation Failed"}, false},
		{&APIError{StatusCode: 404, Message: "Not Found"}, false},
		{&APIError{StatusCode: 401, Message: "Bad credentials"}, false},
		{&APIError{StatusCode: 500, Message: "server error"}, true},
		{&APIError{StatusCode: 429, Message: "too many"}, true},
		{&APIError{StatusCode: 403, Message: "API rate limit exceeded"}, true},
		{&APIError{StatusCode: 403, Message: "Resource not accessible by integration"}, false},
	}
	for _, c := range cases {
		if got := Retryable(c.err); got != c.want {
			t.Fatalf("Retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
