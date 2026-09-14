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

func TestRenderKeepsAttributesStructured(t *testing.T) {
	end := start.Add(time.Minute)
	out, err := otlp.New().Render([]core.Change{{
		Ref: "dep-7", Subject: "acme/prod", Start: start, End: &end,
		Status:     core.StatusCompleted,
		Attributes: map[string]any{"service.version": "2.5.0"},
	}}, now)
	require.NoError(t, err)

	spans := out.([]otlp.Span)
	assert.Equal(t, "2.5.0", spans[0].Attributes["service.version"],
		"OTLP keeps typed attributes; it must NOT inherit Grafana's flattening")
	assert.Equal(t, "acme/prod", spans[0].Attributes["horopter.subject"])
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
