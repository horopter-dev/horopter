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
	"github.com/yo61/horopter/internal/telemetry"
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

	shutdownTelemetry, err := telemetry.Setup(ctx, "horopter-egress")
	if err != nil {
		return err
	}
	defer flushTelemetry(shutdownTelemetry)

	store, err := openStore(ctx, *dsn)
	if err != nil {
		return err
	}
	defer store.Close()

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

	return serve(ctx, srv)
}

// openStore connects without migrating and refuses a schema this build does not
// understand. Only horopter-migrate changes the schema.
func openStore(ctx context.Context, dsn string) (*postgres.Store, error) {
	store, err := postgres.OpenNoMigrate(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := store.AssertSchemaVersion(ctx); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

// serve runs srv until ctx is cancelled or it fails, then drains in-flight requests
// rather than cutting them mid-write on every rollout.
func serve(ctx context.Context, srv *http.Server) error {
	errCh := make(chan error, 1)
	go func() {
		slog.Info("horopter-egress listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("serve on %s: %w", srv.Addr, err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
	}

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

// flushTelemetry drains buffered spans during shutdown.
//
// It deliberately uses a fresh context rather than the one the signal handler has
// already cancelled: the spans still in the batch processor are the ones describing
// whatever caused the shutdown, and flushing them on a cancelled context drops
// exactly the evidence worth keeping. The timeout bounds a collector that is itself
// unreachable, which is a normal way for a shutdown to go wrong.
func flushTelemetry(shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		slog.Warn("flushing telemetry failed; some spans were lost", "error", err)
	}
}
