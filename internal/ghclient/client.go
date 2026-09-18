// Package ghclient is a small, explicit GitHub REST client.
//
// It is hand-rolled rather than go-github because the parts that matter here —
// the two rate limiters, the diff media type, and the 422 behaviour of the
// review API — are exactly the parts a wrapper hides.
package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	base       string
	token      string
	httpClient *http.Client
	userAgent  string
	maxRetries int
}

func New(base, token string) *Client {
	return &Client{
		base:       strings.TrimRight(base, "/"),
		token:      token,
		httpClient: &http.Client{Timeout: 45 * time.Second},
		userAgent:  "pr-sentinel/1.0",
		maxRetries: 4,
	}
}

// APIError carries enough of the response to decide whether a retry is sane.
type APIError struct {
	StatusCode int
	Message    string
	Errors     []string
	Body       string
}

func (e *APIError) Error() string {
	if len(e.Errors) > 0 {
		return fmt.Sprintf("github %d: %s (%s)", e.StatusCode, e.Message, strings.Join(e.Errors, "; "))
	}
	return fmt.Sprintf("github %d: %s", e.StatusCode, e.Message)
}

// IsUnprocessable reports a 422, which for the review API almost always means
// "one of your comments points at a line that is not in the diff".
func (e *APIError) IsUnprocessable() bool { return e.StatusCode == http.StatusUnprocessableEntity }

// Retryable distinguishes a transient failure from a permanent one. A 422 is
// never retryable: the same payload will be rejected identically forever.
func Retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusTooManyRequests:
			return true
		case apiErr.StatusCode >= 500:
			return true
		case apiErr.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(apiErr.Message), "rate limit"):
			return true
		default:
			return false
		}
	}
	// Network-level failures are worth another go.
	return err != nil
}

func (c *Client) do(ctx context.Context, method, path, accept string, body any) ([]byte, *http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with a floor, capped so a job cannot sit here
			// for the whole job timeout.
			wait := time.Duration(math.Pow(2, float64(attempt))) * time.Second
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(wait):
			}
		}

		var reader io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				return nil, nil, err
			}
			reader = bytes.NewReader(raw)
		}

		url := path
		if !strings.HasPrefix(path, "http") {
			url = c.base + path
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reader)
		if err != nil {
			return nil, nil, err
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", c.userAgent)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return raw, resp, nil
		}

		apiErr := parseAPIError(resp.StatusCode, raw)

		// GitHub signals both the primary (hourly) and the secondary (abuse)
		// rate limit with a 403. The primary limit carries a reset timestamp;
		// the secondary carries Retry-After. Honour whichever is present
		// instead of hammering with plain exponential backoff.
		if wait, ok := rateLimitWait(resp); ok && attempt < c.maxRetries {
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(wait):
			}
			lastErr = apiErr
			continue
		}

		if !Retryable(apiErr) || attempt == c.maxRetries {
			return raw, resp, apiErr
		}
		lastErr = apiErr
	}
	return nil, nil, lastErr
}

// rateLimitWait extracts how long to sleep from a throttled response.
func rateLimitWait(resp *http.Response) (time.Duration, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			return capWait(time.Duration(secs) * time.Second), true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			if unix, err := strconv.ParseInt(reset, 10, 64); err == nil {
				d := time.Until(time.Unix(unix, 0))
				if d > 0 {
					return capWait(d), true
				}
			}
		}
	}
	return 0, false
}

// capWait keeps a rate-limit sleep inside the job budget. A full hourly-limit
// reset is longer than any job should hold a worker, so we cap and let the
// retry ladder carry the job into the next window instead.
func capWait(d time.Duration) time.Duration {
	const max = 60 * time.Second
	if d > max {
		return max
	}
	return d
}

func parseAPIError(status int, raw []byte) *APIError {
	e := &APIError{StatusCode: status, Body: string(raw)}
	var payload struct {
		Message string `json:"message"`
		Errors  []struct {
			Resource string `json:"resource"`
			Field    string `json:"field"`
			Code     string `json:"code"`
			Message  string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil {
		e.Message = payload.Message
		for _, x := range payload.Errors {
			msg := x.Message
			if msg == "" {
				msg = fmt.Sprintf("%s.%s %s", x.Resource, x.Field, x.Code)
			}
			e.Errors = append(e.Errors, msg)
		}
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(truncate(string(raw), 200))
	}
	return e
}

// PullRequest is the subset of the PR object the agent needs.
type PullRequest struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			CloneURL string `json:"clone_url"`
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"base"`
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`
	Commits      int `json:"commits"`
}

func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (*PullRequest, error) {
	raw, _, err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number),
		"application/vnd.github+json", nil)
	if err != nil {
		return nil, err
	}
	var pr PullRequest
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, fmt.Errorf("decode pull request: %w", err)
	}
	return &pr, nil
}

// GetPullRequestDiff asks for the unified diff media type rather than
// reconstructing it from the files API, which truncates at 300 files and omits
// the patch for large files.
func (c *Client) GetPullRequestDiff(ctx context.Context, owner, repo string, number int) (string, error) {
	raw, _, err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number),
		"application/vnd.github.v3.diff", nil)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// GetFileContent fetches a file at a ref using the raw media type. This is how
// the agent reads context the diff does not include.
func (c *Client) GetFileContent(ctx context.Context, owner, repo, path, ref string) (string, error) {
	raw, _, err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", owner, repo, path, ref),
		"application/vnd.github.raw", nil)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// SearchCode runs GitHub's code search scoped to one repository.
type CodeSearchResult struct {
	Path       string `json:"path"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func (c *Client) SearchCode(ctx context.Context, owner, repo, query string, limit int) ([]CodeSearchResult, error) {
	q := fmt.Sprintf("%s repo:%s/%s", query, owner, repo)
	raw, _, err := c.do(ctx, http.MethodGet,
		"/search/code?per_page="+strconv.Itoa(limit)+"&q="+urlQueryEscape(q),
		"application/vnd.github+json", nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Items []CodeSearchResult `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload.Items, nil
}

// ReviewComment is one inline comment on a review.
type ReviewComment struct {
	Path string `json:"path"`
	Body string `json:"body"`
	// Line + Side is the modern anchoring form. Position is the legacy form and
	// the two are mutually exclusive — sending both is a 422.
	Line     int    `json:"line,omitempty"`
	Side     string `json:"side,omitempty"`
	Position int    `json:"position,omitempty"`
}

// CreateReviewInput is the payload of POST /pulls/{n}/reviews.
type CreateReviewInput struct {
	CommitID string          `json:"commit_id,omitempty"`
	Body     string          `json:"body"`
	Event    string          `json:"event"` // COMMENT | APPROVE | REQUEST_CHANGES
	Comments []ReviewComment `json:"comments,omitempty"`
}

type Review struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
}

// CreateReview posts a review. It implements the degradation that any real PR
// bot needs: GitHub rejects the entire review with a 422 if even one comment
// is anchored to a line outside the diff, so on that specific failure we drop
// the inline comments and repost the findings in the summary body. A partially
// delivered review beats no review at all.
func (c *Client) CreateReview(ctx context.Context, owner, repo string, number int, in CreateReviewInput) (*Review, error) {
	raw, _, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, number),
		"application/vnd.github+json", in)
	if err == nil {
		var r Review
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return &r, nil
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.IsUnprocessable() || len(in.Comments) == 0 {
		return nil, err
	}

	fallback := in
	fallback.Comments = nil
	fallback.Body = in.Body + "\n\n---\n" +
		"_Inline anchoring failed for this review (GitHub rejected the comment positions), " +
		"so the findings are inlined below instead._\n\n" + renderCommentsAsMarkdown(in.Comments)

	raw2, _, err2 := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, number),
		"application/vnd.github+json", fallback)
	if err2 != nil {
		return nil, fmt.Errorf("review rejected (%v) and summary fallback also failed: %w", apiErr, err2)
	}
	var r Review
	if err := json.Unmarshal(raw2, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func renderCommentsAsMarkdown(cs []ReviewComment) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "**`%s:%d`**\n\n%s\n\n", c.Path, c.Line, c.Body)
	}
	return b.String()
}

// CurrentUser returns the login the token authenticates as, used at startup to
// prove the token works before any job is accepted.
func (c *Client) CurrentUser(ctx context.Context) (string, error) {
	raw, _, err := c.do(ctx, http.MethodGet, "/user", "application/vnd.github+json", nil)
	if err != nil {
		return "", err
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return "", err
	}
	return u.Login, nil
}

func urlQueryEscape(s string) string {
	r := strings.NewReplacer(" ", "+", "\"", "%22", "#", "%23", "&", "%26", "/", "%2F", ":", "%3A")
	return r.Replace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
