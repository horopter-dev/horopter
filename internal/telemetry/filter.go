package telemetry

import (
	"strings"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"
)

// StoreScope is the instrumentation scope for spans the store creates itself, as
// opposed to the per-statement spans otelpgx creates. The two are separately
// switchable because their cost and value differ by an order of magnitude.
const StoreScope = "horopter.dev/horopter/internal/store/postgres"

// areaOf maps an instrumentation scope onto the short area name operators use in
// HOROPTER_TRACE_DISABLE. Operators name the area, not the scope: scope strings are
// long, owned by their libraries, and change with major versions.
//
// An unrecognised scope maps to "", which is never a disabled key, so a library
// added later records by default rather than being silently dropped.
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
//
// embedded.TracerProvider is required by the trace.TracerProvider interface; without
// it this type does not satisfy the interface and does not compile.
type filteringProvider struct {
	embedded.TracerProvider
	inner    trace.TracerProvider
	off      trace.TracerProvider
	disabled map[string]bool
}

// A value receiver is in the pointer's method set, so *filteringProvider satisfies
// trace.TracerProvider either way.
func (p filteringProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	if p.disabled[areaOf(name)] {
		return p.off.Tracer(name, opts...)
	}
	return p.inner.Tracer(name, opts...)
}

// filtered wraps inner so that disabled areas produce non-recording spans — the same
// thing an uninstalled SDK produces, so this composes with Setup rather than adding a
// second way for tracing to be off. With nothing disabled it still wraps, so the
// enabled and disabled paths stay identical in shape.
// Returns a POINTER, not a value. testify's assert.NotSame requires both arguments to be
// pointers and fails outright on a struct value, so telemetry_test.go's check that Setup
// replaced the global provider would fail on the assertion rather than on the thing it
// asserts. Returning a pointer also avoids copying the struct on every Tracer call.
func filtered(inner trace.TracerProvider, disabled map[string]bool) trace.TracerProvider {
	return &filteringProvider{inner: inner, off: noop.NewTracerProvider(), disabled: disabled}
}
