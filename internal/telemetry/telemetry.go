// Package telemetry builds and installs Horopter's OpenTelemetry providers.
//
// A tracer provider exports telemetry spans over OTLP when a collector endpoint is
// configured. A meter provider is installed whenever the SDK is not disabled: its
// Prometheus reader backs the /metrics endpoint, which is on by default, and an OTLP reader
// is added when a collector endpoint is configured.
//
// Nothing else in the codebase decides whether telemetry is on. With no provider installed
// the OTel API returns non-recording spans and no-op instruments, so instrumentation code
// is written unconditionally. OTEL_SDK_DISABLED=true is the one setting under which this
// package installs nothing at all.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// DefaultPrometheusHost and DefaultPrometheusPort are the OpenTelemetry specification's
// defaults for OTEL_EXPORTER_PROMETHEUS_HOST and OTEL_EXPORTER_PROMETHEUS_PORT. They are
// kept rather than chosen for containers, so an operator who knows the specification gets
// what they expect; the README states what that costs inside a container.
const (
	DefaultPrometheusHost = "localhost"
	DefaultPrometheusPort = "9464"
)

// config is the resolved environment. It is a value rather than package state so a
// test can build one directly.
type config struct {
	sdkDisabled    bool
	tracing        bool
	otlpMetrics    bool
	serviceName    string
	disabled       map[string]bool
	prometheusHost string
	prometheusPort string
}

// configFromEnv resolves the standard OTEL_* variables plus this project's own
// HOROPTER_TRACE_DISABLE.
//
// Each OTLP signal is enabled only when an endpoint for it is configured AND the SDK is
// not disabled. OTEL_EXPORTER_OTLP_ENDPOINT counts for both signals; each signal-specific
// variable counts for its own signal only. An absent endpoint counts as off deliberately:
// OTel's own default is localhost:4318, so honouring it would make every run without a
// collector emit export failures that look like a fault and are not one.
//
// The signal-specific variables are the ones the exporters themselves honour in
// preference to the general one, and setting only one is the normal shape when traces and
// metrics go to different collectors. Gating on the general variable alone gave that
// operator a daemon that started happily and emitted nothing.
//
// opentelemetry-go does not implement OTEL_SDK_DISABLED itself — the proposal was
// declined on the grounds that applications can do it — so this reads it directly
// rather than inventing a private name for a variable that already has a standard one.
// It turns off every signal, the /metrics endpoint included.
func configFromEnv(serviceName string) config {
	cfg := config{
		sdkDisabled:    strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true"),
		serviceName:    envOr("OTEL_SERVICE_NAME", serviceName),
		disabled:       disabledAreas(os.Getenv("HOROPTER_TRACE_DISABLE")),
		prometheusHost: envOr("OTEL_EXPORTER_PROMETHEUS_HOST", DefaultPrometheusHost),
		prometheusPort: envOr("OTEL_EXPORTER_PROMETHEUS_PORT", DefaultPrometheusPort),
	}
	general := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
	cfg.tracing = !cfg.sdkDisabled &&
		(general || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "")
	cfg.otlpMetrics = !cfg.sdkDisabled &&
		(general || os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != "")
	return cfg
}

// envOr returns the variable's value, or fallback when it is unset or empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// disabledAreas parses HOROPTER_TRACE_DISABLE's comma-separated area names.
func disabledAreas(raw string) map[string]bool {
	out := map[string]bool{}
	for _, area := range strings.Split(raw, ",") {
		if area = strings.TrimSpace(area); area != "" {
			out[area] = true
		}
	}
	return out
}

// noopShutdown stands in for a tracer provider that was not installed, so Flush never needs
// to nil-check it.
func noopShutdown(context.Context) error { return nil }

// Telemetry is what Setup installed, and how to serve and stop it. A Telemetry whose
// providers are nil installed nothing, and its methods do nothing.
type Telemetry struct {
	cfg            config
	tracerShutdown func(context.Context) error
	meterProvider  *sdkmetric.MeterProvider
	registry       *prometheus.Registry
	metricsServer  *http.Server
}

// Setup installs the providers the environment asks for and returns what it installed.
//
// It returns an error only when the meter provider cannot be built, which in practice means
// its Prometheus reader. The /metrics endpoint is on by default, so an endpoint that is
// meant to be there and silently is not would be "configured but doing nothing"; a daemon
// that refuses to start instead loses no events, because during a rolling deploy the old
// instances keep serving.
//
// Everything that depends on a collector keeps the older rule: a telemetry
// misconfiguration must not stop a system of record. A tracer provider that cannot be built
// is logged at ERROR and the process continues with tracing off, and installMetrics does the
// same for the OTLP metric exporter. It is logged loudly on purpose: a misconfiguration that
// degraded quietly would be indistinguishable from a documented variable that does nothing.
//
// With OTEL_SDK_DISABLED=true it installs nothing and returns a Telemetry whose methods do
// nothing. It never returns a nil Telemetry with a nil error.
func Setup(ctx context.Context, serviceName string) (*Telemetry, error) {
	cfg := configFromEnv(serviceName)
	t := &Telemetry{cfg: cfg, tracerShutdown: noopShutdown}
	if cfg.sdkDisabled {
		return t, nil
	}
	if shutdown, err := installTracing(ctx, cfg); err != nil {
		slog.Error("telemetry setup failed; continuing with tracing off",
			"service", cfg.serviceName, "error", err)
	} else {
		t.tracerShutdown = shutdown
	}
	if err := t.installMetrics(ctx); err != nil {
		return nil, fmt.Errorf("set up metrics for %s: %w", cfg.serviceName, err)
	}
	return t, nil
}

// installTracing builds and installs the tracer provider, returning the error Setup turns
// into a log line. It is separate so the failure has a return value a test can assert on.
func installTracing(ctx context.Context, cfg config) (func(context.Context) error, error) {
	if !cfg.tracing {
		return noopShutdown, nil
	}
	routeDiagnosticsToSlog()

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("create the OTLP trace exporter: %w; check "+
			"OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", err)
	}
	res, err := newResource(cfg.serviceName)
	if err != nil {
		return nil, err
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(filtered(provider, cfg.disabled))
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return provider.Shutdown, nil
}

// routeDiagnosticsToSlog points OpenTelemetry's TWO diagnostic channels at slog. Every
// other failure in these binaries goes through slog, and an operator should not have to
// know that one subsystem reports itself somewhere else in another format.
//
// The channels are separate and carry different things, which is why both are needed.
//
//  1. otel.Handle — run-time failures the SDK considers irremediable, in practice a batch
//     the Collector rejected. Its default handler writes to the stdlib logger on stderr.
//
//  2. otel.SetLogger — the SDK's own internal logger, and the one that carries an
//     OPERATOR'S mistake. Every malformed OTEL_EXPORTER_OTLP_* value is reported here and
//     then ignored: otlptracehttp.New returns a nil error for an unparsable endpoint, an
//     unreadable certificate file, malformed headers, a non-numeric timeout and an unknown
//     protocol alike, falling back to its defaults in each case. Without this, a typo'd
//     endpoint produced one unstructured stderr line and an exporter quietly pointed at
//     localhost — which is the same "configured but doing nothing" failure the endpoint
//     rule in configFromEnv exists to close.
//
// logr's Error maps to slog ERROR, which is what carries those configuration failures.
// The SDK's warn, info and debug chatter goes through V(1), V(4) and V(8), which map
// BELOW slog's Info and stay suppressed at a default level — so this surfaces the
// failures without adding a stream of noise.
//
// Called before any exporter is built, because the exporters are what produce these
// messages: installing the logger afterwards routes the very errors it exists for to
// stderr. installTracing and installMetrics both call it, so it runs whenever either
// installs anything, and never under OTEL_SDK_DISABLED=true.
func routeDiagnosticsToSlog() {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("opentelemetry reported an error", "error", err)
	}))
	otel.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
}

// FlushTimeout bounds the shutdown flush. What goes wrong during a flush is a collector
// that has become unreachable, and this is what stops that from holding up an exit.
const FlushTimeout = 5 * time.Second

// Flush stops the metrics listener and drains buffered telemetry during shutdown. Every
// binary defers it once Setup has succeeded, so the timeout cannot drift between them.
//
// The order matters. The metrics server stops first, so no scrape runs against a provider
// that is shutting down; the meter provider then performs its final OTLP push; the tracer
// provider flushes its batch last.
//
// It builds a fresh context rather than reusing one a signal handler has already
// cancelled: the telemetry still buffered is the telemetry describing whatever caused the
// shutdown, and flushing it on a cancelled context drops exactly the evidence worth keeping.
// Failures are logged, not returned — nothing a shutting-down process could do with them.
//
// Calling it again does nothing: each component is cleared once stopped, because a second
// shutdown of an SDK provider reports an error for work already done.
func (t *Telemetry) Flush() {
	ctx, cancel := context.WithTimeout(context.Background(), FlushTimeout)
	defer cancel()
	if t.metricsServer != nil {
		if err := t.metricsServer.Shutdown(ctx); err != nil {
			slog.Warn("stopping the metrics server failed", "error", err)
		}
		t.metricsServer = nil
	}
	if t.meterProvider != nil {
		if err := t.meterProvider.Shutdown(ctx); err != nil {
			slog.Warn("flushing metrics failed; the last values were lost", "error", err)
		}
		t.meterProvider = nil
	}
	if t.tracerShutdown != nil {
		if err := t.tracerShutdown(ctx); err != nil {
			slog.Warn("flushing telemetry failed; some spans were lost", "error", err)
		}
		t.tracerShutdown = nil
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
