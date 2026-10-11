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

	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/buildinfo"
	"horopter.dev/horopter/internal/ingress"
	"horopter.dev/horopter/internal/logging"
	"horopter.dev/horopter/internal/store/postgres"
	"horopter.dev/horopter/internal/telemetry"
)

func main() {
	code, handled := buildinfo.Handle("horopter-ingress", os.Args[1:], os.Stdout, os.Stderr)
	if handled {
		os.Exit(code)
	}
	if err := run(); err != nil {
		slog.Error("horopter-ingress failed", "error", err)
		os.Exit(1)
	}
}

type ingressConfig struct {
	addr                  string
	dsn                   string
	maxAttributeBytes     int
	allowLocusPrefixedRef bool
}

func setupIngressConfig() (ingressConfig, error) {
	addr := flag.String("addr", envOr("HOROPTER_ADDR", "0.0.0.0:8080"), "listen address")
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	flag.Parse()

	if *dsn == "" {
		return ingressConfig{}, errors.New("DATABASE_URL is required, e.g. " +
			"postgres://user:pass@host:5432/horopter")
	}

	maxAttributeBytes, err := ingress.MaxAttributeBytesFromEnv()
	if err != nil {
		return ingressConfig{}, err
	}

	allowLocusPrefixedRef, err := ingress.AllowLocusPrefixedRefFromEnv()
	if err != nil {
		return ingressConfig{}, err
	}

	return ingressConfig{
		addr:                  *addr,
		dsn:                   *dsn,
		maxAttributeBytes:     maxAttributeBytes,
		allowLocusPrefixedRef: allowLocusPrefixedRef,
	}, nil
}

func run() error {
	if err := logging.Setup(); err != nil {
		return err
	}

	cfg, err := setupIngressConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tel, err := telemetry.Setup(ctx, "horopter-ingress")
	if err != nil {
		return err
	}
	defer tel.Flush()

	if err := tel.ServeMetrics(); err != nil {
		return err
	}

	store, err := postgres.OpenNoMigrate(ctx, cfg.dsn,
		postgres.WithMaxEventAttributeBytes(cfg.maxAttributeBytes))
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.AssertSchemaVersion(ctx); err != nil {
		return err
	}

	// DefaultOptions, never a zero Options: an uncached authenticator queries the
	// database on every request, ahead of anything that could shed it.
	authn := auth.New(auth.NewStore(store.Pool()), time.Now, auth.DefaultOptions())

	// ingress.New rejects a nil authenticator, so this is checked exactly once, in the
	// constructor, rather than here as well: two guards on one fact drift.
	api, err := ingress.New(store, time.Now, rand.Reader, authn,
		ingress.WithAllowLocusPrefixedRef(cfg.allowLocusPrefixedRef))
	if err != nil {
		return err
	}

	return serve(ctx, &http.Server{
		Addr:    cfg.addr,
		Handler: api.Routes(),
		// auth.MaxHeaderBytes bounds the request line plus headers, Authorization
		// included — see its own doc comment for why Go's 1 MiB default is worth
		// tightening, and why one constant serves both daemons.
		MaxHeaderBytes: auth.MaxHeaderBytes,
		// ReadHeaderTimeout bounds a slowloris header dribble. ReadTimeout bounds the
		// whole request including the body, so a client sending one byte a minute cannot
		// hold a connection open indefinitely; 15s is far longer than a few-kilobyte
		// event needs over any real network.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		// WriteTimeout bounds handler time plus the response write. An append is one
		// short transaction, and the longest a handler can legitimately block is
		// postgres.AppendLockTimeout waiting behind a rebuild, so this must comfortably
		// exceed that or a retryable 503 would be replaced by a dropped connection.
		WriteTimeout: 30 * time.Second,
		// IdleTimeout bounds a kept-alive connection between requests, so idle clients
		// cannot accumulate file descriptors.
		IdleTimeout: 120 * time.Second,
	})
}

// serve runs srv until ctx is cancelled or it fails, then drains in-flight requests
// rather than cutting them mid-write on every rollout.
func serve(ctx context.Context, srv *http.Server) error {
	errCh := make(chan error, 1)
	go func() {
		slog.Info("horopter-ingress listening", "addr", srv.Addr)
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
