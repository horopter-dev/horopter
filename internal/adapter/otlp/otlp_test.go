package otlp_test

import (
	"encoding/json"
	"fmt"
	"strconv"
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

// spansOf reaches through the OTLP envelope to the one ResourceSpans and
// ScopeSpans every Render call produces, and returns its spans.
func spansOf(t *testing.T, out any) []otlp.Span {
	t.Helper()
	req, ok := out.(otlp.ExportRequest)
	require.True(t, ok, "Render must return otlp.ExportRequest")
	require.Len(t, req.ResourceSpans, 1)
	require.Len(t, req.ResourceSpans[0].ScopeSpans, 1)
	return req.ResourceSpans[0].ScopeSpans[0].Spans
}

func TestRenderUsesNanosecondsNotMilliseconds(t *testing.T) {
	end := start.Add(20 * time.Minute)
	out, err := otlp.New().Render([]core.Change{{
		Ref: "dep-7", Subject: "acme/prod", Type: "deployment", Title: "API v2.5.0",
		Start: start, End: &end, Status: core.StatusCompleted,
		Attributes: map[string]any{"service.version": "2.5.0"},
	}}, now)
	require.NoError(t, err)

	span := spansOf(t, out)[0]
	assert.Equal(t, strconv.FormatInt(start.UnixNano(), 10), span.StartTimeUnixNano,
		"OTLP uses nanoseconds where Grafana uses milliseconds — proof the unit lives in the adapter")
	assert.Equal(t, strconv.FormatInt(end.UnixNano(), 10), span.EndTimeUnixNano)
}

// TestRenderMapsAttributesToTypedAnyValue pins that each Go attribute type maps
// onto the specific AnyValue union member OTLP defines for it, not a flattened
// string: a string-only fixture can't distinguish "kept typed" from "flattened
// to a string", since flattening a string is the identity operation.
func TestRenderMapsAttributesToTypedAnyValue(t *testing.T) {
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

	byKey := attrsByKey(t, spansOf(t, out)[0].Attributes)

	require.NotNil(t, byKey["service.version"].StringValue)
	assert.Equal(t, "2.5.0", *byKey["service.version"].StringValue)

	require.NotNil(t, byKey["cicd.pipeline.run.attempt"].IntValue,
		"an int attribute must become intValue")
	assert.Equal(t, "3", *byKey["cicd.pipeline.run.attempt"].IntValue,
		"intValue is a decimal string, per the protobuf JSON mapping for 64-bit integers")

	require.NotNil(t, byKey["deployment.rollback"].BoolValue)
	assert.True(t, *byKey["deployment.rollback"].BoolValue)

	require.NotNil(t, byKey["service.metadata"].KvlistValue,
		"a nested map attribute must become a kvlistValue, not a flattened string")
	nested := byKey["service.metadata"].KvlistValue.Values
	require.Len(t, nested, 1)
	assert.Equal(t, "region", nested[0].Key)
	require.NotNil(t, nested[0].Value.StringValue)
	assert.Equal(t, "us-east-1", *nested[0].Value.StringValue)
}

// TestRenderFallsBackToStringForUnmappedAttributeTypes pins the documented
// fallback for a Go value with no clean AnyValue counterpart (see AnyValue's
// doc comment): its fmt "%v" string form, not a render failure.
func TestRenderFallsBackToStringForUnmappedAttributeTypes(t *testing.T) {
	type custom struct{ N int }
	end := start.Add(time.Minute)
	out, err := otlp.New().Render([]core.Change{{
		Ref: "dep-9", Start: start, End: &end, Status: core.StatusCompleted,
		Attributes: map[string]any{"exotic": custom{N: 7}},
	}}, now)
	require.NoError(t, err)

	byKey := attrsByKey(t, spansOf(t, out)[0].Attributes)
	require.NotNil(t, byKey["exotic"].StringValue)
	assert.Equal(t, "{7}", *byKey["exotic"].StringValue)
}

// TestRenderMapsNilAttributeToEmptyAnyValue pins the other half of the
// fallback rule: a Go nil is OTLP's own "no value" case, not the misleading
// string "<nil>".
func TestRenderMapsNilAttributeToEmptyAnyValue(t *testing.T) {
	end := start.Add(time.Minute)
	out, err := otlp.New().Render([]core.Change{{
		Ref: "dep-9", Start: start, End: &end, Status: core.StatusCompleted,
		Attributes: map[string]any{"absent": nil},
	}}, now)
	require.NoError(t, err)

	byKey := attrsByKey(t, spansOf(t, out)[0].Attributes)
	assert.Equal(t, otlp.AnyValue{}, byKey["absent"],
		"a Go nil must map to a wholly empty AnyValue, not a stringified \"<nil>\"")
}

func attrsByKey(t *testing.T, attrs []otlp.KeyValue) map[string]otlp.AnyValue {
	t.Helper()
	byKey := make(map[string]otlp.AnyValue, len(attrs))
	for _, a := range attrs {
		byKey[a.Key] = a.Value
	}
	return byKey
}

func TestRenderMapsStatusToOTelStatus(t *testing.T) {
	end := start.Add(time.Minute)
	for _, tc := range []struct {
		in   core.Status
		code int
		set  bool
	}{
		{core.StatusCompleted, 1, true},
		{core.StatusFailed, 2, true},
		{core.StatusRolledBack, 2, true},
		{core.StatusOccurred, 0, false},
		{core.StatusInProgress, 0, false},
		{core.StatusCancelled, 0, false},
	} {
		out, err := otlp.New().Render([]core.Change{{
			Ref: "r", Start: start, End: &end, Status: tc.in,
		}}, now)
		require.NoError(t, err)
		span := spansOf(t, out)[0]
		if !tc.set {
			assert.Nil(t, span.Status, "status %s must be omitted (OTLP's unset)", tc.in)
			continue
		}
		require.NotNil(t, span.Status, "status %s", tc.in)
		assert.Equal(t, tc.code, span.Status.Code, "status %s", tc.in)
	}
}

func TestRenderOpenChangeEndsAtNow(t *testing.T) {
	out, err := otlp.New().Render([]core.Change{{
		Ref: "dep-8", Start: start, End: nil, Status: core.StatusInProgress,
	}}, now)
	require.NoError(t, err)
	assert.Equal(t, strconv.FormatInt(now.UnixNano(), 10), spansOf(t, out)[0].EndTimeUnixNano)
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

	span1 := spansOf(t, out1)[0]
	span2 := spansOf(t, out2)[0]
	assert.Equal(t, span1.TraceID, span2.TraceID, "re-exporting must not duplicate trace ids")
	assert.Equal(t, span1.SpanID, span2.SpanID, "re-exporting must not duplicate span ids")
}

// TestRenderIDsDependOnTenant pins that two changes differing only by
// tenant must produce different span ids AND different trace ids. Before the
// fix, SpanID derived from ref alone, so two tenants sharing a ref collided.
func TestRenderIDsDependOnTenant(t *testing.T) {
	end := start.Add(time.Minute)
	a, err := otlp.New().Render([]core.Change{{
		Tenant: "acme", Ref: "dep-7", Start: start, End: &end, Status: core.StatusCompleted,
	}}, now)
	require.NoError(t, err)
	b, err := otlp.New().Render([]core.Change{{
		Tenant: "other", Ref: "dep-7", Start: start, End: &end, Status: core.StatusCompleted,
	}}, now)
	require.NoError(t, err)

	spanA := spansOf(t, a)[0]
	spanB := spansOf(t, b)[0]
	assert.NotEqual(t, spanA.TraceID, spanB.TraceID,
		"two tenants sharing a ref must not collide on trace id")
	assert.NotEqual(t, spanA.SpanID, spanB.SpanID,
		"two tenants sharing a ref must not collide on span id")
}

// TestRenderSpanIDIsNotATraceIDPrefix guards the domain-separation half of that:
// trace id and span id must be independent derivations, not the same hash
// truncated at two lengths, or the 8-byte span id would always be a prefix of
// the 16-byte trace id — an accidental correlation nobody asked for.
func TestRenderSpanIDIsNotATraceIDPrefix(t *testing.T) {
	end := start.Add(time.Minute)
	out, err := otlp.New().Render([]core.Change{{
		Tenant: "acme", Ref: "dep-7", Start: start, End: &end, Status: core.StatusCompleted,
	}}, now)
	require.NoError(t, err)

	span := spansOf(t, out)[0]
	assert.NotEqual(t, span.SpanID, span.TraceID[:len(span.SpanID)],
		"trace id and span id must be domain-separated, independent derivations")
}

// TestRenderGoldenOTLPJSON asserts the exact marshalled JSON
// shape — nesting, lowerCamelCase field names, and specifically that the
// nanosecond fields serialise as strings — rather than asserting against the
// Go struct, which cannot catch a missing or wrong json tag.
func TestRenderGoldenOTLPJSON(t *testing.T) {
	end := start.Add(20 * time.Minute)
	out, err := otlp.New().Render([]core.Change{{
		Tenant: "acme", Ref: "dep-7", Subject: "acme/prod", Type: "deployment",
		Title: "API v2.5.0", Start: start, End: &end, Status: core.StatusCompleted,
		Attributes: map[string]any{"service.version": "2.5.0"},
	}}, now)
	require.NoError(t, err)

	b, err := json.Marshal(out)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(b, &decoded))
	span := decodedSpan(t, decoded)

	startRaw, ok := span["startTimeUnixNano"].(string)
	require.True(t, ok, "startTimeUnixNano must be a JSON string, not a number")
	assert.Equal(t, strconv.FormatInt(start.UnixNano(), 10), startRaw)

	traceID, _ := span["traceId"].(string)
	spanID, _ := span["spanId"].(string)
	assert.Regexp(t, `^[0-9a-f]{32}$`, traceID)
	assert.Regexp(t, `^[0-9a-f]{16}$`, spanID)

	expected := fmt.Sprintf(`{
		"resourceSpans": [{
			"resource": {"attributes": [
				{"key": "service.name", "value": {"stringValue": "horopter"}}
			]},
			"scopeSpans": [{
				"spans": [{
					"traceId": %q,
					"spanId": %q,
					"name": "API v2.5.0",
					"startTimeUnixNano": "%d",
					"endTimeUnixNano": "%d",
					"attributes": [
						{"key": "horopter.ref", "value": {"stringValue": "dep-7"}},
						{"key": "horopter.status", "value": {"stringValue": "completed"}},
						{"key": "horopter.subject", "value": {"stringValue": "acme/prod"}},
						{"key": "service.version", "value": {"stringValue": "2.5.0"}}
					],
					"status": {"code": 1}
				}]
			}]
		}]
	}`, traceID, spanID, start.UnixNano(), end.UnixNano())
	assert.JSONEq(t, expected, string(b))
}

func decodedSpan(t *testing.T, decoded map[string]any) map[string]any {
	t.Helper()
	resourceSpans, ok := decoded["resourceSpans"].([]any)
	require.True(t, ok, "resourceSpans must be a JSON array")
	require.Len(t, resourceSpans, 1)
	scopeSpans, ok := resourceSpans[0].(map[string]any)["scopeSpans"].([]any)
	require.True(t, ok, "scopeSpans must be a JSON array")
	require.Len(t, scopeSpans, 1)
	spans, ok := scopeSpans[0].(map[string]any)["spans"].([]any)
	require.True(t, ok, "spans must be a JSON array")
	require.Len(t, spans, 1)
	span, ok := spans[0].(map[string]any)
	require.True(t, ok)
	return span
}

// Render(nil, now) must still emit a well-formed envelope whose spans array is
// [], never null: a real OTLP collector's JSON receiver expects the envelope
// shape unconditionally, and a bare null spans list can break one.
func TestRenderNilChangesSpansArrayIsEmptyNotNull(t *testing.T) {
	out, err := otlp.New().Render(nil, now)
	require.NoError(t, err)

	b, err := json.Marshal(out)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"resourceSpans": [{
			"resource": {"attributes": [
				{"key": "service.name", "value": {"stringValue": "horopter"}}
			]},
			"scopeSpans": [{"spans": []}]
		}]
	}`, string(b))
}
