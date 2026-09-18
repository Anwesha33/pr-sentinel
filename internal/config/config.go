// Package config loads all runtime configuration from the environment.
//
// Every knob has a default that works against the docker-compose stack, so a
// developer can run `make up && go run ./cmd/api` with an empty environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr string

	PostgresDSN string

	RedisAddr     string
	RedisPassword string
	RedisDB       int

	KafkaBrokers       []string
	ReviewTopic        string
	ReviewDLQTopic     string
	ConsumerGroup      string
	MaxDeliveryAttempt int

	GitHubToken   string
	GitHubAPIBase string
	// DryRun is the safety default: the agent computes the full review payload
	// and persists it, but never calls the GitHub write API.
	DryRun bool

	GeminiAPIKey string
	GeminiModel  string
	LLMTimeout   time.Duration

	// Agent limits — an agent without a budget is an outage waiting to happen.
	MaxAgentSteps    int
	MaxDiffBytes     int
	MaxFileReadBytes int
	WorkspaceRoot    string

	WorkerConcurrency int
	JobTimeout        time.Duration
}

func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:           env("HTTP_ADDR", ":8080"),
		PostgresDSN:        env("POSTGRES_DSN", "postgres://prsentinel:prsentinel@localhost:5432/prsentinel?sslmode=disable"),
		RedisAddr:          env("REDIS_ADDR", "localhost:6379"),
		RedisPassword:      env("REDIS_PASSWORD", ""),
		RedisDB:            envInt("REDIS_DB", 0),
		KafkaBrokers:       strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
		ReviewTopic:        env("KAFKA_REVIEW_TOPIC", "pr.review.requested"),
		ReviewDLQTopic:     env("KAFKA_REVIEW_DLQ_TOPIC", "pr.review.dlq"),
		ConsumerGroup:      env("KAFKA_CONSUMER_GROUP", "pr-sentinel-workers"),
		MaxDeliveryAttempt: envInt("MAX_DELIVERY_ATTEMPTS", 4),
		GitHubToken:        env("GITHUB_TOKEN", ""),
		GitHubAPIBase:      env("GITHUB_API_BASE", "https://api.github.com"),
		DryRun:             envBool("DRY_RUN", true),
		GeminiAPIKey:       env("GEMINI_API_KEY", ""),
		GeminiModel:        env("GEMINI_MODEL", "gemini-flash-latest"),
		LLMTimeout:         envDur("LLM_TIMEOUT", 90*time.Second),
		MaxAgentSteps:      envInt("MAX_AGENT_STEPS", 12),
		MaxDiffBytes:       envInt("MAX_DIFF_BYTES", 200000),
		MaxFileReadBytes:   envInt("MAX_FILE_READ_BYTES", 60000),
		WorkspaceRoot:      env("WORKSPACE_ROOT", os.TempDir()+"/pr-sentinel"),
		WorkerConcurrency:  envInt("WORKER_CONCURRENCY", 4),
		JobTimeout:         envDur("JOB_TIMEOUT", 10*time.Minute),
	}
	if c.MaxAgentSteps < 1 {
		return nil, fmt.Errorf("MAX_AGENT_STEPS must be >= 1")
	}
	return c, nil
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}
