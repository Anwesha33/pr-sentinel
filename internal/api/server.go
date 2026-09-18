// Package api is the HTTP surface: submit a review, check its status, read the
// agent's trace, and receive GitHub webhooks.
//
// The API never runs a review itself. It validates, persists a job, publishes
// to Kafka and returns 202. That separation is the point of the architecture:
// a GitHub webhook must be answered in milliseconds, while a review takes
// minutes.
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Anwesha33/pr-sentinel/internal/config"
	"github.com/Anwesha33/pr-sentinel/internal/queue"
	"github.com/Anwesha33/pr-sentinel/internal/store"
)

type Server struct {
	cfg           *config.Config
	store         *store.Store
	producer      *queue.Producer
	log           *slog.Logger
	webhookSecret string
	startedAt     time.Time
}

func NewServer(cfg *config.Config, st *store.Store, p *queue.Producer, log *slog.Logger, webhookSecret string) *Server {
	return &Server{cfg: cfg, store: st, producer: p, log: log, webhookSecret: webhookSecret, startedAt: time.Now()}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/reviews", s.createReview)
	mux.HandleFunc("GET /v1/reviews", s.listReviews)
	mux.HandleFunc("GET /v1/reviews/{id}", s.getReview)
	mux.HandleFunc("GET /v1/reviews/{id}/steps", s.getSteps)
	mux.HandleFunc("POST /webhooks/github", s.githubWebhook)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	return s.withLogging(mux)
}

type createReviewRequest struct {
	// Either repo_url ("https://github.com/owner/repo/pull/12") or the explicit
	// triple. The URL form exists because it is what a human pastes.
	RepoURL  string `json:"repo_url,omitempty"`
	Owner    string `json:"owner,omitempty"`
	Repo     string `json:"repo,omitempty"`
	PRNumber int    `json:"pr_number,omitempty"`
	DryRun   *bool  `json:"dry_run,omitempty"`
	// IdempotencyKey lets a caller retry safely. When absent we derive one from
	// the pull request coordinates, so double-submitting the same PR reuses the
	// in-flight job instead of starting a second review.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type createReviewResponse struct {
	JobID     uuid.UUID `json:"job_id"`
	Status    string    `json:"status"`
	Deduped   bool      `json:"deduped"`
	DryRun    bool      `json:"dry_run"`
	StatusURL string    `json:"status_url"`
}

func (s *Server) createReview(w http.ResponseWriter, r *http.Request) {
	var req createReviewRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}

	owner, repo, number := req.Owner, req.Repo, req.PRNumber
	if req.RepoURL != "" {
		o, rp, n, err := ParsePullRequestURL(req.RepoURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		owner, repo, number = o, rp, n
	}
	if owner == "" || repo == "" || number <= 0 {
		writeError(w, http.StatusBadRequest, "provide repo_url, or owner + repo + pr_number")
		return
	}

	dryRun := s.cfg.DryRun
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}

	key := req.IdempotencyKey
	if key == "" {
		key = fmt.Sprintf("%s/%s#%d", owner, repo, number)
	}

	job, created, err := s.store.CreateJob(r.Context(), &store.Job{
		IdempotencyKey: key,
		RepoOwner:      owner,
		RepoName:       repo,
		PRNumber:       number,
		DryRun:         dryRun,
		MaxAttempts:    s.cfg.MaxDeliveryAttempt,
	})
	if err != nil {
		s.log.Error("create job", "err", err)
		writeError(w, http.StatusInternalServerError, "could not create job")
		return
	}

	if created {
		if err := s.producer.Publish(r.Context(), s.cfg.ReviewTopic, queue.ReviewRequest{
			JobID: job.ID, RepoOwner: owner, RepoName: repo, PRNumber: number,
			DryRun: dryRun, Attempt: 1, EnqueuedAt: time.Now(),
		}); err != nil {
			// The job row exists but nothing will pick it up. Say so honestly
			// rather than returning 202 for work that will never happen; the
			// stale-job reclaimer will also rescue it.
			s.log.Error("publish review request", "err", err, "job_id", job.ID)
			writeError(w, http.StatusServiceUnavailable, "job recorded but could not be queued; it will be retried")
			return
		}
	}

	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, createReviewResponse{
		JobID:     job.ID,
		Status:    job.Status,
		Deduped:   !created,
		DryRun:    job.DryRun,
		StatusURL: "/v1/reviews/" + job.ID.String(),
	})
}

type reviewDetail struct {
	Job      *store.Job          `json:"job"`
	Findings []store.Finding     `json:"findings"`
	Review   *store.PostedReview `json:"review,omitempty"`
}

func (s *Server) getReview(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	job, err := s.store.JobByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such job")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	findings, err := s.store.FindingsByJob(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	detail := reviewDetail{Job: job, Findings: findings}
	if posted, err := s.store.PostedReviewByJob(r.Context(), id); err == nil {
		detail.Review = posted
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) getSteps(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	steps, err := s.store.StepsByJob(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job_id": id, "steps": steps})
}

func (s *Server) listReviews(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	jobs, err := s.store.ListJobs(r.Context(), r.URL.Query().Get("status"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "count": len(jobs)})
}

// githubWebhook accepts pull_request events. Only `opened`, `reopened` and
// `synchronize` (a new push to the branch) are reviewable; everything else is
// acknowledged and ignored, because returning a non-2xx to GitHub gets the
// webhook disabled after enough failures.
func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable body")
		return
	}
	if s.webhookSecret != "" {
		if !ValidSignature(s.webhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
			writeError(w, http.StatusUnauthorized, "bad signature")
			return
		}
	}

	if r.Header.Get("X-GitHub-Event") != "pull_request" {
		writeJSON(w, http.StatusOK, map[string]string{"ignored": "not a pull_request event"})
		return
	}

	var payload struct {
		Action      string `json:"action"`
		Number      int    `json:"number"`
		PullRequest struct {
			Head struct {
				SHA string `json:"sha"`
			} `json:"head"`
			Draft bool `json:"draft"`
		} `json:"pull_request"`
		Repository struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "malformed payload")
		return
	}

	switch payload.Action {
	case "opened", "reopened", "synchronize":
	default:
		writeJSON(w, http.StatusOK, map[string]string{"ignored": "action " + payload.Action})
		return
	}
	if payload.PullRequest.Draft {
		writeJSON(w, http.StatusOK, map[string]string{"ignored": "draft pull request"})
		return
	}

	owner := payload.Repository.Owner.Login
	repo := payload.Repository.Name
	// The head SHA is in the idempotency key so that a re-delivered webhook is
	// deduplicated, but a genuine new push still gets its own review.
	key := fmt.Sprintf("%s/%s#%d@%s", owner, repo, payload.Number, payload.PullRequest.Head.SHA)

	job, created, err := s.store.CreateJob(r.Context(), &store.Job{
		IdempotencyKey: key, RepoOwner: owner, RepoName: repo, PRNumber: payload.Number,
		HeadSHA: payload.PullRequest.Head.SHA, DryRun: s.cfg.DryRun, MaxAttempts: s.cfg.MaxDeliveryAttempt,
	})
	if err != nil {
		s.log.Error("webhook create job", "err", err)
		writeError(w, http.StatusInternalServerError, "could not record job")
		return
	}
	if created {
		if err := s.producer.Publish(r.Context(), s.cfg.ReviewTopic, queue.ReviewRequest{
			JobID: job.ID, RepoOwner: owner, RepoName: repo, PRNumber: payload.Number,
			DryRun: job.DryRun, Attempt: 1, EnqueuedAt: time.Now(),
		}); err != nil {
			s.log.Error("webhook publish", "err", err)
			writeError(w, http.StatusServiceUnavailable, "could not queue")
			return
		}
	}
	writeJSON(w, http.StatusAccepted, createReviewResponse{
		JobID: job.ID, Status: job.Status, Deduped: !created, DryRun: job.DryRun,
		StatusURL: "/v1/reviews/" + job.ID.String(),
	})
}

// ValidSignature verifies GitHub's HMAC-SHA256 webhook signature in constant
// time. Comparing with == here would leak the digest a byte at a time.
func ValidSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// ParsePullRequestURL accepts the URL a human copies out of the browser.
func ParsePullRequestURL(raw string) (owner, repo string, number int, err error) {
	trimmed := strings.TrimSpace(raw)
	for _, p := range []string{"https://", "http://", "github.com/", "www."} {
		trimmed = strings.TrimPrefix(trimmed, p)
	}
	trimmed = strings.TrimPrefix(trimmed, "github.com/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) < 4 || (parts[2] != "pull" && parts[2] != "pulls") {
		return "", "", 0, fmt.Errorf("expected a URL like https://github.com/owner/repo/pull/123, got %q", raw)
	}
	n, convErr := strconv.Atoi(strings.Split(parts[3], "#")[0])
	if convErr != nil {
		return "", "", 0, fmt.Errorf("%q is not a pull request number", parts[3])
	}
	return parts[0], parts[1], n, nil
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "uptime_seconds": int(time.Since(s.startedAt).Seconds()),
	})
}

// readyz checks the dependencies the API genuinely needs to accept work.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "degraded", "postgres": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
