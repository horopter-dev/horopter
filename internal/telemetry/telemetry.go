// Package telemetry builds and installs Horopter's OpenTelemetry providers.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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

// DefaultPrometheusHost and DefaultPrometheusPort are the OpenTelemetry specification's defaults.
const (
	DefaultPrometheusHost = "localhost"
	DefaultPrometheusPort = "9464"
)

// config is the resolved environment.
type config struct {
	sdkDisabled    bool
	tracing        bool
	otlpMetrics    bool
	serviceName    string
	disabled       map[string]bool
	prometheusHost string
	prometheusPort string
}

// configFromEnv resolves the OTEL_* variables; opentelemetry-go ignores OTEL_SDK_DISABLED.
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

// noopShutdown stands in for a tracer provider that was not installed.
func noopShutdown(context.Context) error { return nil }

// Telemetry is what Setup installed, and how to serve and stop it.
type Telemetry struct {
	cfg            config
	tracerShutdown func(context.Context) error
	meterProvider  *sdkmetric.MeterProvider
	registry       *prometheus.Registry
	metricsServer  *http.Server
	// Close sees the listener only once the Serve goroutine has registered it.
	metricsListener net.Listener
}

// Setup installs the providers the environment asks for; only a meter provider failure errors.
func Setup(ctx context.Context, serviceName string) (*Telemetry, error) {
	cfg := configFromEnv(serviceName)
	t := &Telemetry{cfg: cfg, tracerShutdown: noopShutdown}
	if cfg.sdkDisabled {
		return t, nil
	}
	// Before installTracing: a tracer provider installed first leaked when this failed.
	if err := t.installMetrics(ctx); err != nil {
		return nil, fmt.Errorf("set up metrics for %s: %w", cfg.serviceName, err)
	}
	if shutdown, err := installTracing(ctx, cfg); err != nil {
		slog.Error("telemetry setup failed; continuing with tracing off",
			"service", cfg.serviceName, "error", err)
	} else {
		t.tracerShutdown = shutdown
	}
	return t, nil
}

// installTracing builds and installs the tracer provider, returning the error Setup logs.
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

// routeDiagnosticsToSlog must run before any exporter is built: exporters log config errors.
func routeDiagnosticsToSlog() {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("opentelemetry reported an error", "error", err)
	}))
	otel.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
}

// FlushTimeout bounds the shutdown flush, so an unreachable collector cannot hold up an exit.
const FlushTimeout = 5 * time.Second

// Flush stops /metrics and drains buffered telemetry on a fresh context; repeat calls do nothing.
func (t *Telemetry) Flush() {
	ctx, cancel := context.WithTimeout(context.Background(), FlushTimeout)
	defer cancel()
	// Close, not Shutdown: an idle /metrics connection must not spend the shared budget.
	t.stopMetricsEndpoint()
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

// stopMetricsEndpoint closes the server before the listener, or every shutdown logs an error.
func (t *Telemetry) stopMetricsEndpoint() {
	if t.metricsServer != nil {
		if err := t.metricsServer.Close(); err != nil {
			slog.Warn("stopping the metrics server failed", "error", err)
		}
		t.metricsServer = nil
	}
	if t.metricsListener != nil {
		// Serve also closes the listener on its way out.
		if err := t.metricsListener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Warn("closing the metrics listener failed", "error", err)
		}
		t.metricsListener = nil
	}
}

// newResource identifies this process by service.name.
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
