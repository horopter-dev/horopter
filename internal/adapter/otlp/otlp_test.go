package otlp_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/adapter/otlp"
	"github.com/yo61/horopter/internal/core"
)

var (
	start = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	now   = time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)
)

func TestRenderUsesNanosecondsNotMilliseconds(t *testing.T) {
	end := start.Add(20 * time.Minute)
	out, err := otlp.New().Render([]core.Change{{
		Ref: "dep-7", Subject: "acme/prod", Type: "deployment", Title: "API v2.5.0",
		Start: start, End: &end, Status: core.StatusCompleted,
		Attributes: map[string]any{"service.version": "2.5.0"},
	}}, now)
	require.NoError(t, err)

	spans := out.([]otlp.Span)
	require.Len(t, spans, 1)
	assert.Equal(t, start.UnixNano(), spans[0].StartUnixNano,
		"OTLP uses nanoseconds where Grafana uses milliseconds — proof the unit lives in the adapter")
	assert.Equal(t, end.UnixNano(), spans[0].EndUnixNano)
}

// A string-only fixture can't distinguish "kept typed" from "flattened to a
// string": flattening a string is the identity operation. This fixture adds
// an int, a bool, and a nested map — each becomes a different string under
// Grafana-style "%v" flattening — and asserts the concrete Go type survives,
// not just the rendered value, so a flattening regression fails a type
// assertion rather than slipping through a same-string comparison.
func TestRenderKeepsAttributesStructured(t *testing.T) {
	end := start.Add(time.Minute)
	out, err := otlp.New().Render([]core.Change{{
		Ref: "dep-7", Subject: "acme/prod", Start: start, End: &end,
		Status: core.StatusCompleted,
		Attributes: map[string]any{
			"service.version":           "2.5.0",
			"cicd.pipeline.run.attempt": 3,
			"deployment.rollback":       true,
			"service.metadata":          map[string]any{"region": "us-east-1"},
		},
	}}, now)
	require.NoError(t, err)

	spans := out.([]otlp.Span)
	attrs := spans[0].Attributes
	assert.Equal(t, "2.5.0", attrs["service.version"],
		"OTLP keeps typed attributes; it must NOT inherit Grafana's flattening")
	assert.Equal(t, "acme/prod", attrs["horopter.subject"])

	attempt, ok := attrs["cicd.pipeline.run.attempt"].(int)
	require.True(t, ok, "int attribute must survive as int, not be flattened to a string")
	assert.Equal(t, 3, attempt)

	rollback, ok := attrs["deployment.rollback"].(bool)
	require.True(t, ok, "bool attribute must survive as bool, not be flattened to a string")
	assert.True(t, rollback)

	metadata, ok := attrs["service.metadata"].(map[string]any)
	require.True(t, ok, "nested map attribute must survive as map[string]any, not a string")
	assert.Equal(t, "us-east-1", metadata["region"])
}

func TestRenderMapsStatusToOTelStatus(t *testing.T) {
	end := start.Add(time.Minute)
	for _, tc := range []struct {
		in   core.Status
		want string
	}{
		{core.StatusCompleted, "Ok"},
		{core.StatusFailed, "Error"},
		{core.StatusRolledBack, "Error"},
		{core.StatusOccurred, "Unset"},
		{core.StatusInProgress, "Unset"},
		{core.StatusCancelled, "Unset"},
	} {
		out, err := otlp.New().Render([]core.Change{{
			Ref: "r", Start: start, End: &end, Status: tc.in,
		}}, now)
		require.NoError(t, err)
		assert.Equal(t, tc.want, out.([]otlp.Span)[0].Status, "status %s", tc.in)
	}
}

func TestRenderOpenChangeEndsAtNow(t *testing.T) {
	out, err := otlp.New().Render([]core.Change{{
		Ref: "dep-8", Start: start, End: nil, Status: core.StatusInProgress,
	}}, now)
	require.NoError(t, err)
	assert.Equal(t, now.UnixNano(), out.([]otlp.Span)[0].EndUnixNano)
}

// Re-exporting the same change must yield the same span and trace ids rather
// than duplicates, so that OTel backends can deduplicate correctly.
func TestRenderIDsAreStableAcrossReRender(t *testing.T) {
	end := start.Add(time.Minute)
	change := core.Change{
		Tenant: "acme", Ref: "dep-7", Start: start, End: &end, Status: core.StatusCompleted,
	}
	out1, err := otlp.New().Render([]core.Change{change}, now)
	require.NoError(t, err)
	out2, err := otlp.New().Render([]core.Change{change}, now)
	require.NoError(t, err)

	span1 := out1.([]otlp.Span)[0]
	span2 := out2.([]otlp.Span)[0]
	assert.Equal(t, span1.TraceID, span2.TraceID, "re-exporting must not duplicate trace ids")
	assert.Equal(t, span1.SpanID, span2.SpanID, "re-exporting must not duplicate span ids")
}

// Render(nil, now) must marshal to a JSON array, never null, since Grafana's
// (and any OTel collector's) contract expects an array — see grafana_test.go
// for the equivalent guard.
func TestRenderNilChangesMarshalsToEmptyArray(t *testing.T) {
	out, err := otlp.New().Render(nil, now)
	require.NoError(t, err)

	b, err := json.Marshal(out)
	require.NoError(t, err)
	assert.JSONEq(t, `[]`, string(b))
}
