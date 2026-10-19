package ingress

import (
	"strings"
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2/event"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
)

var (
	ceOccurred = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	ceNow      = time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC)
)

var testPrincipal = auth.Principal{
	Tenant: core.TenantIDFromRow("tnt-test"),
	Locus:  "github",
	Origin: core.OriginProcess,
}

func ceWithData(t *testing.T, data map[string]any) cloudevents.Event {
	t.Helper()
	e := cloudevents.New()
	e.SetID("ce-1")
	e.SetSource("/buildkite/pipelines/api")
	e.SetType("deployment.started")
	e.SetSubject("acme/prod")
	e.SetTime(ceOccurred)
	require.NoError(t, e.SetData("application/json", data))
	return e
}

// Every pair below collides under a plain join, a bare length prefix, or a colon join.
func TestIdempotencyKeyIsInjective(t *testing.T) {
	pairs := []struct{ source, id string }{
		{"github/repo", "123"},
		{"github", "repo/123"},
		{"github/repo/123", ""},
		{"", "github/repo/123"},
		{"a", "1:b"},
		{"a:1", "b"},
		{"1", "0:x"},
		{"10", ":x"},
		{"/buildkite/pipelines/api", "build-7"},
		{"/buildkite/pipelines", "/api/build-7"},
	}

	seen := map[string]int{}
	for i, p := range pairs {
		key := idempotencyKey(p.source, p.id)
		if prev, clash := seen[key]; clash {
			t.Errorf("source %q + id %q collides with source %q + id %q on key %q; "+
				"two distinct events would share one idempotency key and the second "+
				"would be silently swallowed",
				p.source, p.id, pairs[prev].source, pairs[prev].id, key)
			continue
		}
		seen[key] = i
	}
	assert.Len(t, seen, len(pairs), "every distinct pair must produce a distinct key")

	// Not NUL: PostgreSQL TEXT cannot store 0x00.
	for _, p := range pairs {
		assert.NotContains(t, idempotencyKey(p.source, p.id), "\x00",
			"the key is stored in a TEXT column, which cannot hold a NUL byte")
	}
}

// Each lifted key must also leave attributes, so one value is not stored twice.
func TestEventFromCloudEventLiftsEveryRecognisedDataKey(t *testing.T) {
	ce := ceWithData(t, map[string]any{
		"correlation_ref": "dep-7",
		"title":           "Deploy api",
		"actor":           "alice",
		"lifecycle":       "started",
		"region":          "eu-west-1",
	})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal, false)
	require.NoError(t, err)

	assert.Equal(t, "github/dep-7", ev.CorrelationRef.String(),
		"correlation_ref must be lifted out of data and prefixed with the locus")
	assert.Equal(t, "Deploy api", ev.Title, "title must be lifted out of data")
	assert.Equal(t, "alice", ev.Actor, "actor must be lifted out of data")
	assert.Equal(t, core.LifecycleStarted, ev.Lifecycle, "lifecycle must be lifted out of data")

	assert.Equal(t, map[string]any{"region": "eu-west-1"}, ev.Attributes,
		"a lifted key must not remain in attributes, and an unrecognised key must stay")
}

func TestEventFromCloudEventWithoutActorIsValid(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal, false)
	require.NoError(t, err)
	assert.Empty(t, ev.Actor, "an event carrying no actor is accepted with an empty one")
}

func TestEventFromCloudEventLeavesNonStringRecognisedKeysAsAttributes(t *testing.T) {
	ce := ceWithData(t, map[string]any{"title": 7, "actor": true, "lifecycle": 7})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal, false)
	require.NoError(t, err)

	assert.Empty(t, ev.Title, "a non-string title must not be coerced into the field")
	assert.Empty(t, ev.Actor, "a non-string actor must not be coerced into the field")
	assert.Empty(t, ev.Lifecycle, "a non-string lifecycle must not be coerced into the field")
	assert.Equal(t,
		map[string]any{"title": float64(7), "actor": true, "lifecycle": float64(7)},
		ev.Attributes,
		"a value the field cannot hold must survive as the attribute it was sent as")
}

func TestEventFromCloudEventTakesTenantFromThePrincipal(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal, false)
	require.NoError(t, err)
	assert.Equal(t, testPrincipal.Tenant.String(), ev.Tenant)
}

// Silently dropping a once-authoritative field hides a broken migration, so refuse it.
func TestEventFromCloudEventRejectsDataCarryingTenant(t *testing.T) {
	for name, tenant := range map[string]any{"string": "other", "number": 7, "null": nil} {
		t.Run(name, func(t *testing.T) {
			ce := ceWithData(t, map[string]any{"tenant": tenant, "correlation_ref": "dep-7"})
			_, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal, false)
			require.Error(t, err)
			assert.ErrorIs(t, err, core.ErrInvalidEvent)
			assert.Contains(t, err.Error(), "tenant is derived from the credential")
		})
	}
}

func TestEventFromCloudEventSetsOriginFromThePrincipal(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal, false)
	require.NoError(t, err)
	assert.Equal(t, core.OriginProcess, ev.Origin)
}

func TestEventFromCloudEventSetsLocusFromThePrincipal(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal, false)
	require.NoError(t, err)
	assert.Equal(t, testPrincipal.Locus, ev.Locus.String())
}

// Must wrap ErrInvalidEvent, or appendFailed would report it as a retryable 503.
func TestEventFromCloudEventRejectsARefOverTheCap(t *testing.T) {
	ce := ceWithData(t, map[string]any{
		"correlation_ref": strings.Repeat("x", auth.MaxRefBytes-len("github/")+1),
	})

	_, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidEvent)
	assert.Contains(t, err.Error(), "ref")
}

// Composing first would hide a leading space: "github/ dep-1".
func TestEventFromCloudEventRejectsARefWithLeadingWhitespaceEvenAfterTheLocusPrefix(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": " dep-1"})

	_, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidEvent)
	assert.Contains(t, err.Error(), "whitespace")
}

func TestEventFromCloudEventDataOmittingTenantIsAccepted(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	_, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal, false)
	assert.NoError(t, err)
}

// A zero occurred_at would drag the ref's first_seen_at minimum to year 1.
func TestEventFromCloudEventFallsBackToReceiptTimeWithoutAnEnvelopeTime(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})
	ce.SetTime(time.Time{})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal, false)
	require.NoError(t, err)

	assert.Equal(t, ceNow, ev.OccurredAt,
		"an envelope with no time must fall back to receipt time, not to the zero time")
	assert.False(t, ev.OccurredAt.IsZero(), "a zero occurred_at would poison first_seen_at")
}

func TestEventFromCloudEventKeepsAnEnvelopeTime(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal, false)
	require.NoError(t, err)
	assert.Equal(t, ceOccurred, ev.OccurredAt, "a stated occurrence time must be preserved")
}
