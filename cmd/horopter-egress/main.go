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

	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/buildinfo"
	"horopter.dev/horopter/internal/egress"
	"horopter.dev/horopter/internal/logging"
	"horopter.dev/horopter/internal/store/postgres"
	"horopter.dev/horopter/internal/telemetry"
)

func main() {
	code, handled := buildinfo.Handle("horopter-egress", os.Args[1:], os.Stdout, os.Stderr)
	if handled {
		os.Exit(code)
	}
	if err := run(); err != nil {
		slog.Error("horopter-egress failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if err := logging.Setup(); err != nil {
		return err
	}

	addr := flag.String("addr", envOr("HOROPTER_ADDR", "0.0.0.0:8081"), "listen address")
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	flag.Parse()

	if *dsn == "" {
		return errors.New("DATABASE_URL is required, e.g. " +
			"postgres://user:pass@host:5432/horopter")
	}

	limits, err := egress.LimitsFromEnv()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tel, err := telemetry.Setup(ctx, "horopter-egress")
	if err != nil {
		return err
	}
	defer tel.Flush()

	if err := tel.ServeMetrics(); err != nil {
		return err
	}

	store, err := openStore(ctx, *dsn)
	if err != nil {
		return err
	}
	defer store.Close()

	// DefaultOptions, never a zero Options: an uncached authenticator queries the database.
	authn := auth.New(auth.NewStore(store.Pool()), time.Now, auth.DefaultOptions())

	api, err := egress.New(store, time.Now, limits, authn)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: api.Routes(),

		MaxHeaderBytes: auth.MaxHeaderBytes,
		// LimitsFromEnv keeps both admission waits well inside WriteTimeout.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return serve(ctx, srv)
}

// openStore connects without migrating and refuses a schema this build does not understand.
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

// serve runs srv until ctx is cancelled or it fails, then drains in-flight requests.
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
