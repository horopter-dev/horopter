package telemetry

import (
	"strings"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"
)

// StoreScope is the instrumentation scope for spans the store creates itself.
const StoreScope = "horopter.dev/horopter/internal/store/postgres"

// areaOf maps a scope onto its HOROPTER_TRACE_DISABLE area; an unknown scope records.
func areaOf(scope string) string {
	switch {
	case strings.HasSuffix(scope, "/otelhttp"):
		return "http"
	case scope == "github.com/exaring/otelpgx":
		return "sql"
	case scope == StoreScope:
		return "store"
	default:
		return ""
	}
}

// filteringProvider returns a no-op tracer for any scope whose area is disabled.
type filteringProvider struct {
	embedded.TracerProvider
	inner    trace.TracerProvider
	off      trace.TracerProvider
	disabled map[string]bool
}

func (p filteringProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	if p.disabled[areaOf(name)] {
		return p.off.Tracer(name, opts...)
	}
	return p.inner.Tracer(name, opts...)
}

// filtered returns a pointer because assert.NotSame fails outright on a struct value.
func filtered(inner trace.TracerProvider, disabled map[string]bool) trace.TracerProvider {
	return &filteringProvider{inner: inner, off: noop.NewTracerProvider(), disabled: disabled}
}
