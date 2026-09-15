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
	"os"
	"strings"

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
	cfg.enabled = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" &&
		!strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true")
	return cfg
}

// noopShutdown is returned whenever no provider was installed, so callers never
// need to nil-check the shutdown function.
func noopShutdown(context.Context) error { return nil }

// Setup installs the global tracer provider and returns a function that flushes and
// stops it. When tracing is disabled it installs nothing and returns a shutdown that
// does nothing; callers treat both cases identically.
//
// The returned shutdown must be called with a FRESH context, not the one cancelled by
// the signal handler: flushing needs time after the shutdown signal, and a cancelled
// context drops exactly the spans describing whatever caused the shutdown.
func Setup(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	cfg := configFromEnv(serviceName)
	if !cfg.enabled {
		return noopShutdown, nil
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return noopShutdown, fmt.Errorf(
			"create the OTLP trace exporter: %w; check OTEL_EXPORTER_OTLP_ENDPOINT", err)
	}
	res, err := newResource(cfg.serviceName)
	if err != nil {
		return noopShutdown, err
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return provider.Shutdown, nil
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
