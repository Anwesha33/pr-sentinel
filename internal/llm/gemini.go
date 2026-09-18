// Package llm wraps the Gemini generateContent API with the two things the
// agent depends on: function calling and honest token accounting.
//
// The transport is plain REST rather than an SDK so that the request shape —
// in particular how a tool result is fed back into the conversation — is
// visible in this file rather than buried behind a builder.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultEndpoint = "https://generativelanguage.googleapis.com/v1beta"

type Client struct {
	apiKey   string
	model    string
	endpoint string
	http     *http.Client
	// maxRetries covers 429 and 5xx, which Gemini returns readily under load.
	maxRetries int
	// retryBudget caps the total time spent retrying one call. Without it the
	// backoff ladder plus a server-supplied retryDelay can park a worker for a
	// quarter of an hour, which looks exactly like a hang and burns the job
	// timeout doing nothing. Failing at the budget hands the problem to the
	// job's own retry ladder, which is the layer designed to wait minutes.
	retryBudget time.Duration
}

func New(apiKey, model string, timeout time.Duration) *Client {
	return &Client{
		apiKey:      apiKey,
		model:       model,
		endpoint:    defaultEndpoint,
		http:        &http.Client{Timeout: timeout},
		maxRetries:  6,
		retryBudget: 90 * time.Second,
	}
}

func (c *Client) Model() string { return c.model }

// Role values accepted by the API. Tool results are sent back as a `user`
// turn carrying a functionResponse part — there is no separate "tool" role.
const (
	RoleUser  = "user"
	RoleModel = "model"
)

type Part struct {
	Text             string            `json:"text,omitempty"`
	FunctionCall     *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`

	// ThoughtSignature is an opaque token the model attaches to reasoning and
	// function-call parts. Recent Gemini models reject the next turn with 400
	// unless the signature is echoed back exactly as received, so the agent
	// must append the model's own Content to history rather than rebuilding an
	// equivalent-looking one. Thought is the matching flag on reasoning parts.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
	Thought          bool   `json:"thought,omitempty"`
}

type Content struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts"`
}

type FunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type FunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

// FunctionDeclaration describes a tool. Parameters is a JSON Schema subset:
// type/properties/required/description/enum/items.
type FunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type GenerationConfig struct {
	Temperature      float64 `json:"temperature,omitempty"`
	MaxOutputTokens  int     `json:"maxOutputTokens,omitempty"`
	ResponseMIMEType string  `json:"responseMimeType,omitempty"`
}

type Request struct {
	System   string
	Contents []Content
	Tools    []FunctionDeclaration
	Config   GenerationConfig
	// ForceJSON asks for application/json output. It cannot be combined with
	// tools — the API rejects that pairing — so the agent turns tools off for
	// the final structured answer.
	ForceJSON bool
}

type Usage struct {
	PromptTokens int `json:"promptTokenCount"`
	OutputTokens int `json:"candidatesTokenCount"`
	TotalTokens  int `json:"totalTokenCount"`
}

type Response struct {
	Text          string
	FunctionCalls []FunctionCall
	FinishReason  string
	Usage         Usage
	Latency       time.Duration

	// RawContent is the candidate's Content exactly as the API returned it.
	// Append this to the conversation verbatim; see Part.ThoughtSignature.
	RawContent Content
}

// ErrNoCandidates means the model returned nothing usable — usually a safety
// block. It is treated as a permanent failure for that prompt.
var ErrNoCandidates = errors.New("llm returned no candidates")

type apiRequest struct {
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	Contents          []Content         `json:"contents"`
	Tools             []apiTool         `json:"tools,omitempty"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
}

type apiTool struct {
	FunctionDeclarations []FunctionDeclaration `json:"functionDeclarations"`
}

type apiResponse struct {
	Candidates []struct {
		Content      Content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata  Usage `json:"usageMetadata"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback,omitempty"`
}

func (c *Client) Generate(ctx context.Context, req Request) (*Response, error) {
	if c.apiKey == "" {
		return nil, errors.New("GEMINI_API_KEY is not set")
	}

	body := apiRequest{Contents: req.Contents}
	if req.System != "" {
		body.SystemInstruction = &Content{Parts: []Part{{Text: req.System}}}
	}
	if len(req.Tools) > 0 {
		body.Tools = []apiTool{{FunctionDeclarations: req.Tools}}
	}
	cfg := req.Config
	if req.ForceJSON {
		if len(req.Tools) > 0 {
			return nil, errors.New("ForceJSON cannot be combined with tools")
		}
		cfg.ResponseMIMEType = "application/json"
	}
	if cfg != (GenerationConfig{}) {
		body.GenerationConfig = &cfg
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", c.endpoint, c.model, c.apiKey)
	// Errors from net/http embed the full request URL, which for this API
	// carries the API key as a query parameter. Anything that formats such an
	// error into a log line publishes the key, so every error that can contain
	// a URL goes through redact() before it leaves this package.

	var lastErr error
	deadline := time.Now().Add(c.retryBudget)
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("gemini retry budget of %s exhausted after %d attempts: %w",
					c.retryBudget, attempt, lastErr)
			}
			// Gemini returns 503 "high demand" readily on shared capacity, and
			// those spikes last tens of seconds, so the ladder climbs to 30s
			// rather than giving up inside the first ten.
			wait := time.Duration(1<<attempt) * time.Second
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}

		start := time.Now()
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(httpReq)
		if err != nil {
			lastErr = c.redact(err)
			continue
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("gemini %d: %s", resp.StatusCode, truncate(string(payload), 300))
			// A 429 carries a RetryInfo telling us exactly how long the quota
			// window has left. Honouring it beats the blind backoff ladder:
			// retrying early just burns another request against the same quota.
			if d, ok := retryDelay(payload); ok && time.Now().Add(d).Before(deadline) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(d):
				}
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			// 400/403 will not improve on retry (bad key, bad request shape).
			return nil, fmt.Errorf("gemini %d: %s", resp.StatusCode, truncate(string(payload), 500))
		}

		var ar apiResponse
		if err := json.Unmarshal(payload, &ar); err != nil {
			return nil, fmt.Errorf("decode gemini response: %w", err)
		}
		if len(ar.Candidates) == 0 {
			reason := ""
			if ar.PromptFeedback != nil {
				reason = ar.PromptFeedback.BlockReason
			}
			return nil, fmt.Errorf("%w (block reason: %q)", ErrNoCandidates, reason)
		}

		cand := ar.Candidates[0]
		out := &Response{
			FinishReason: cand.FinishReason,
			Usage:        ar.UsageMetadata,
			Latency:      time.Since(start),
			RawContent:   cand.Content,
		}
		if out.RawContent.Role == "" {
			out.RawContent.Role = RoleModel
		}
		var sb strings.Builder
		for _, p := range cand.Content.Parts {
			// Reasoning parts are not part of the visible answer.
			if p.Thought {
				continue
			}
			if p.Text != "" {
				sb.WriteString(p.Text)
			}
			if p.FunctionCall != nil {
				out.FunctionCalls = append(out.FunctionCalls, *p.FunctionCall)
			}
		}
		out.Text = sb.String()
		return out, nil
	}
	return nil, fmt.Errorf("gemini request failed after %d attempts: %w", c.maxRetries+1, lastErr)
}

// redact removes the API key from an error's text. net/http wraps transport
// failures in a *url.Error that includes the full request URL, key and all.
func (c *Client) redact(err error) error {
	if err == nil || c.apiKey == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, c.apiKey) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, c.apiKey, "REDACTED"))
}

// retryDelay reads google.rpc.RetryInfo out of an error payload. The delay is
// capped so a day-long quota exhaustion fails fast instead of hanging a worker.
func retryDelay(payload []byte) (time.Duration, bool) {
	var e struct {
		Error struct {
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &e); err != nil {
		return 0, false
	}
	for _, d := range e.Error.Details {
		if !strings.HasSuffix(d.Type, "RetryInfo") || d.RetryDelay == "" {
			continue
		}
		// The field is a protobuf Duration rendered as "41s" or "1.5s".
		dur, err := time.ParseDuration(d.RetryDelay)
		if err != nil {
			continue
		}
		const max = 60 * time.Second
		if dur > max {
			return 0, false
		}
		return dur, true
	}
	return 0, false
}

// ExtractJSON pulls a JSON object or array out of a model response that may be
// wrapped in prose or a ```json fence. Models do this often enough that
// tolerating it is cheaper than a retry.
func ExtractJSON(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if fence := strings.Index(s, "```"); fence >= 0 {
		rest := s[fence+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			s = strings.TrimSpace(rest[:end])
		}
	}
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return "", false
	}
	open := s[start]
	closeCh := byte('}')
	if open == '[' {
		closeCh = ']'
	}
	depth, inStr, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inStr:
			escaped = true
		case ch == '"':
			inStr = !inStr
		case inStr:
			// skip
		case ch == open:
			depth++
		case ch == closeCh:
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
