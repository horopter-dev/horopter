package telemetry

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func recorderProvider() (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	rec := tracetest.NewSpanRecorder()
	return sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)), rec
}

func TestFilteredSuppressesADisabledArea(t *testing.T) {
	inner, rec := recorderProvider()
	p := filtered(inner, map[string]bool{"sql": true})

	_, span := p.Tracer("github.com/exaring/otelpgx").Start(context.Background(), "query")
	span.End()

	assert.Empty(t, rec.Ended(), "a disabled area must produce no spans")
}

func TestFilteredLeavesOtherAreasRecording(t *testing.T) {
	inner, rec := recorderProvider()
	p := filtered(inner, map[string]bool{"sql": true})

	_, span := p.Tracer(StoreScope).Start(context.Background(), "AppendEvent")
	span.End()

	assert.Len(t, rec.Ended(), 1, "disabling sql must not disable store")
}

func TestFilteredPassesUnknownScopesThrough(t *testing.T) {
	inner, rec := recorderProvider()
	p := filtered(inner, map[string]bool{"sql": true})

	_, span := p.Tracer("example.com/some/other/library").Start(context.Background(), "x")
	span.End()

	assert.Len(t, rec.Ended(), 1,
		"a scope this project does not know must record rather than be silently dropped")
}

func TestAreaOf(t *testing.T) {
	for scope, want := range map[string]string{
		"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp": "http",
		"github.com/exaring/otelpgx":                                    "sql",
		StoreScope:                                                      "store",
		"example.com/unknown":                                           "",
	} {
		assert.Equal(t, want, areaOf(scope), scope)
	}
}
