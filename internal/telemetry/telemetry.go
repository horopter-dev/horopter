// Package telemetry builds and installs Horopter's OpenTelemetry tracer provider.
//
// Nothing else in the codebase decides whether tracing is on. With no provider
// installed the OTel API returns non-recording spans, so instrumentation code is
// written unconditionally and "off" means this package installed nothing. That is
// why no instrumented function anywhere carries a tracing conditional.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// config is the resolved environment. It is a value rather than package state so a
// test can build one directly.
type config struct {
	enabled     bool
	serviceName string
	disabled    map[string]bool
}

// configFromEnv resolves the standard OTEL_* variables plus this project's own
// HOROPTER_TRACE_DISABLE.
//
// Tracing is enabled only when an endpoint is configured AND the SDK is not
// disabled. An absent endpoint counts as off deliberately: OTel's own default is
// localhost:4318, so honouring it would make every run without a collector emit
// export failures that look like a fault and are not one.
//
// Either endpoint variable counts. OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is the
// signal-specific form the exporter itself honours in preference to the general one,
// and it is the normal shape when traces and metrics go to different collectors.
// Gating on the general variable alone gave an operator who set only the
// signal-specific one a daemon that started happily and emitted nothing.
//
// opentelemetry-go does not implement OTEL_SDK_DISABLED itself — the proposal was
// declined on the grounds that applications can do it — so this reads it directly
// rather than inventing a private name for a variable that already has a standard one.
func configFromEnv(serviceName string) config {
	cfg := config{
		serviceName: serviceName,
		disabled:    map[string]bool{},
	}
	if name := os.Getenv("OTEL_SERVICE_NAME"); name != "" {
		cfg.serviceName = name
	}
	for _, area := range strings.Split(os.Getenv("HOROPTER_TRACE_DISABLE"), ",") {
		if area = strings.TrimSpace(area); area != "" {
			cfg.disabled[area] = true
		}
	}
	endpointSet := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
	cfg.enabled = endpointSet && !strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true")
	return cfg
}

// noopShutdown is returned whenever no provider was installed, so callers never
// need to nil-check the shutdown function.
func noopShutdown(context.Context) error { return nil }

// Setup installs the global tracer provider and returns a function that flushes and
// stops it. When tracing is disabled it installs nothing and returns a shutdown that
// does nothing; callers treat both cases identically.
//
// A telemetry misconfiguration must not stop a system of record. Losing events is the
// worst failure Horopter has and traces are secondary, so a provider that cannot be
// built is logged at ERROR and the process continues with tracing off rather than
// refusing to start. It is logged loudly on purpose: a misconfiguration that degraded
// quietly to no-tracing would be indistinguishable from a documented variable that does
// nothing, which is the defect this whole configuration surface exists to avoid.
//
// The returned shutdown must be called with a FRESH context, not the one cancelled by
// the signal handler: flushing needs time after the shutdown signal, and a cancelled
// context drops exactly the spans describing whatever caused the shutdown. Flush does
// that; callers should use it rather than calling the returned function directly.
func Setup(ctx context.Context, serviceName string) func(context.Context) error {
	cfg := configFromEnv(serviceName)
	shutdown, err := install(ctx, cfg)
	if err != nil {
		slog.Error("telemetry setup failed; continuing with tracing off",
			"service", cfg.serviceName, "error", err)
		return noopShutdown
	}
	return shutdown
}

// install builds and installs the provider, returning the error Setup turns into a log
// line. It is separate so the failure has a return value a test can assert on.
func install(ctx context.Context, cfg config) (func(context.Context) error, error) {
	if !cfg.enabled {
		return noopShutdown, nil
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"create the OTLP trace exporter: %w; check OTEL_EXPORTER_OTLP_ENDPOINT", err)
	}
	res, err := newResource(cfg.serviceName)
	if err != nil {
		return nil, err
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	// Export failures reach otel.Handle, whose default handler writes to the stdlib
	// logger on stderr. Every other failure in these binaries goes through slog, so a
	// collector that starts rejecting spans should not be the one thing an operator has
	// to find in a different log format. Installed only on the enabled path, so
	// "disabled means this package installed nothing" stays literally true.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("opentelemetry reported an error", "error", err)
	}))
	otel.SetTracerProvider(filtered(provider, cfg.disabled))
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return provider.Shutdown, nil
}

// FlushTimeout bounds the shutdown flush. What goes wrong during a flush is a collector
// that has become unreachable, and this is what stops that from holding up an exit.
const FlushTimeout = 5 * time.Second

// Flush drains buffered spans during shutdown. It is the counterpart to Setup and every
// binary calls it the same way, so the timeout cannot drift between them.
//
// It deliberately builds a fresh context rather than reusing the one a signal handler
// has already cancelled: the spans still in the batch processor are the ones describing
// whatever caused the shutdown, and flushing them on a cancelled context drops exactly
// the evidence worth keeping.
func Flush(shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), FlushTimeout)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		slog.Warn("flushing telemetry failed; some spans were lost", "error", err)
	}
}

// newResource identifies this process. service.name is what a backend groups by, so
// it is set explicitly rather than left to the SDK's "unknown_service" fallback.
func newResource(serviceName string) (*resource.Resource, error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("build the telemetry resource for %q: %w", serviceName, err)
	}
	return res, nil
}
