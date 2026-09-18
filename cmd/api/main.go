// Command api accepts review requests and hands them to Kafka. It never runs a
// review itself; see cmd/worker.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Anwesha33/pr-sentinel/internal/api"
	"github.com/Anwesha33/pr-sentinel/internal/config"
	"github.com/Anwesha33/pr-sentinel/internal/queue"
	"github.com/Anwesha33/pr-sentinel/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openStoreWithRetry(ctx, cfg.PostgresDSN, log)
	if err != nil {
		log.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}

	// Topics are created here rather than in the worker so that a request can
	// be accepted even if no worker has ever started.
	if err := queue.EnsureTopics(ctx, cfg.KafkaBrokers,
		queue.AllTopics(cfg.ReviewTopic, cfg.ReviewDLQTopic), 3); err != nil {
		log.Warn("could not pre-create topics; relying on auto-creation", "err", err)
	}

	producer := queue.NewProducer(cfg.KafkaBrokers)
	defer producer.Close()

	srv := api.NewServer(cfg, st, producer, log, os.Getenv("GITHUB_WEBHOOK_SECRET"))
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	go func() {
		log.Info("api listening", "addr", cfg.HTTPAddr, "dry_run_default", cfg.DryRun)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
	}
}

// openStoreWithRetry tolerates Postgres not being up yet, which is the normal
// state of affairs for the first few seconds of `docker compose up`.
func openStoreWithRetry(ctx context.Context, dsn string, log *slog.Logger) (*store.Store, error) {
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		st, err := store.Open(ctx, dsn)
		if err == nil {
			return st, nil
		}
		lastErr = err
		log.Info("waiting for postgres", "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, lastErr
}
