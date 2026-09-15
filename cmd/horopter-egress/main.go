// Command horopter-egress is the read-side daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yo61/horopter/internal/egress"
	"github.com/yo61/horopter/internal/store/postgres"
)

func main() {
	if err := run(); err != nil {
		slog.Error("horopter-egress failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", envOr("HOROPTER_ADDR", "0.0.0.0:8081"), "listen address")
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	flag.Parse()

	if *dsn == "" {
		return errors.New("DATABASE_URL is required, e.g. " +
			"postgres://user:pass@host:5432/horopter")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := postgres.OpenNoMigrate(ctx, *dsn)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.AssertSchemaVersion(ctx); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: egress.New(store, time.Now).Routes(),
		// ReadHeaderTimeout bounds a slowloris header dribble. ReadTimeout bounds the
		// whole request; every egress route is a GET carrying no body, so 15s is already
		// generous. WriteTimeout bounds handler time plus the response write, which for a
		// MaxLimit result set is one indexed query and a JSON encode. IdleTimeout bounds
		// a kept-alive connection between requests so idle clients cannot accumulate file
		// descriptors. Without the last three a client could hold a connection forever.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("horopter-egress listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("serve on %s: %w", *addr, err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	// Drain in-flight requests rather than cutting them mid-read on every rollout.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
