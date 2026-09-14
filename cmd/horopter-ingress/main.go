// Command horopter-ingress is the write-side daemon.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yo61/horopter/internal/ingress"
	"github.com/yo61/horopter/internal/store/postgres"
)

func main() {
	if err := run(); err != nil {
		slog.Error("horopter-ingress failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", envOr("HOROPTER_ADDR", "0.0.0.0:8080"), "listen address")
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	flag.Parse()

	if *dsn == "" {
		return errors.New("DATABASE_URL is required, e.g. " +
			"postgres://user:pass@host:5432/horopter")
	}
	token := os.Getenv("HOROPTER_INGRESS_TOKEN")
	if token == "" {
		return errors.New("HOROPTER_INGRESS_TOKEN is required; ingress accepts writes " +
			"and must not run unauthenticated")
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
		Addr:              *addr,
		Handler:           ingress.New(store, time.Now, rand.Reader, token).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("horopter-ingress listening", "addr", *addr)
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

	// Drain in-flight requests rather than cutting them mid-write on every rollout.
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
