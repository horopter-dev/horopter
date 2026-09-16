package ingress

import (
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2/event"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	ceOccurred = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	ceNow      = time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC)
)

// ceWithData builds a structured CloudEvent carrying data as its JSON payload.
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

// TestIdempotencyKeyIsInjective pins the property the encoding exists for: distinct
// (source, id) pairs never collide. CloudEvents source is a URI-reference, so embedded
// slashes are ordinary, and a plain "source/id" join makes "github/repo" + "123"
// indistinguishable from "github" + "repo/123". The consequence of that collision is
// identical to the one the composite key exists to remove — a distinct event silently
// swallowed and answered 200 {"created": false} — so narrowing the collision class is not
// the same as closing it.
//
// The pairs below are chosen so that every one of them collides under a plain join, or
// under a length prefix with no delimiter, or under a colon join. Comparing every pair
// against every other is what makes the test a statement about the encoding rather than
// about four hand-picked examples.
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

	// A NUL separator would be injective too, and is deliberately not used: PostgreSQL
	// TEXT cannot store 0x00, so such a key would fail at the INSERT instead of deduping.
	for _, p := range pairs {
		assert.NotContains(t, idempotencyKey(p.source, p.id), "\x00",
			"the key is stored in a TEXT column, which cannot hold a NUL byte")
	}
}

// TestEventFromCloudEventLiftsEveryRecognisedDataKey pins the whole lifted set at once:
// tenant, correlation_ref, title and actor each become a first-class event field, and
// each is REMOVED from attributes so one value is not stored twice under two names. An
// unrecognised key stays in attributes untouched.
//
// The mapping previously had no assertions at all. Mutating any of the four pulls to a
// constant "", or deleting the delete(attrs, key) line, passed the entire suite.
func TestEventFromCloudEventLiftsEveryRecognisedDataKey(t *testing.T) {
	ce := ceWithData(t, map[string]any{
		"tenant":          "acme",
		"correlation_ref": "dep-7",
		"title":           "Deploy api",
		"actor":           "alice",
		"region":          "eu-west-1",
	})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A")
	require.NoError(t, err)

	assert.Equal(t, "acme", ev.Tenant, "tenant must be lifted out of data")
	assert.Equal(t, "dep-7", ev.CorrelationRef, "correlation_ref must be lifted out of data")
	assert.Equal(t, "Deploy api", ev.Title, "title must be lifted out of data")
	assert.Equal(t, "alice", ev.Actor, "actor must be lifted out of data")

	assert.Equal(t, map[string]any{"region": "eu-west-1"}, ev.Attributes,
		"a lifted key must not remain in attributes, and an unrecognised key must stay")
}

// Actor reaches the event only through the data payload: CloudEvents has no envelope
// attribute for it, and POST /v1/events carries it as a first-class request field. An
// actorless CloudEvent is normal rather than invalid, so it yields an empty actor and
// no error.
func TestEventFromCloudEventWithoutActorIsValid(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A")
	require.NoError(t, err)
	assert.Empty(t, ev.Actor, "an event carrying no actor is accepted with an empty one")
}

// A recognised key holding a non-string value is left in attributes rather than coerced.
// Stringifying it would put a number into a field the schema promises is a title, and
// silently remove the caller's own attribute in the process.
func TestEventFromCloudEventLeavesNonStringRecognisedKeysAsAttributes(t *testing.T) {
	ce := ceWithData(t, map[string]any{"title": 7, "actor": true})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A")
	require.NoError(t, err)

	assert.Empty(t, ev.Title, "a non-string title must not be coerced into the field")
	assert.Empty(t, ev.Actor, "a non-string actor must not be coerced into the field")
	assert.Equal(t, map[string]any{"title": float64(7), "actor": true}, ev.Attributes,
		"a value the field cannot hold must survive as the attribute it was sent as")
}

// An absent tenant resolves to DefaultTenant rather than to the empty string, so a
// producer that omits it writes somewhere queryable instead of into a nameless bucket.
func TestEventFromCloudEventDefaultsTenantWhenDataOmitsIt(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A")
	require.NoError(t, err)
	assert.Equal(t, DefaultTenant, ev.Tenant)
}

// The CloudEvents time attribute is OPTIONAL, so an envelope without one must still
// produce a usable occurred_at. Receipt time is the honest fallback: it is the earliest
// moment the system can attest to. Without the fallback the event carries a zero time,
// and a zero occurred_at drags first_seen_at — a running minimum — to year 1 for the
// whole ref.
func TestEventFromCloudEventFallsBackToReceiptTimeWithoutAnEnvelopeTime(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})
	ce.SetTime(time.Time{})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A")
	require.NoError(t, err)

	assert.Equal(t, ceNow, ev.OccurredAt,
		"an envelope with no time must fall back to receipt time, not to the zero time")
	assert.False(t, ev.OccurredAt.IsZero(), "a zero occurred_at would poison first_seen_at")
}

// An envelope that DOES carry a time keeps it: the fallback must not overwrite a stated
// occurrence with receipt time, or every event would look like it happened on arrival.
func TestEventFromCloudEventKeepsAnEnvelopeTime(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A")
	require.NoError(t, err)
	assert.Equal(t, ceOccurred, ev.OccurredAt, "a stated occurrence time must be preserved")
}
