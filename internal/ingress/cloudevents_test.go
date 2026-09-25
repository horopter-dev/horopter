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

// testPrincipal is the credential eventFromCloudEvent derives tenant, locus and origin
// from — package ingress's tests build one directly rather than going through auth's
// resolution, which internal/auth already covers.
var testPrincipal = auth.Principal{
	Tenant: core.TenantIDFromRow("tnt-test"),
	Locus:  "github",
	Origin: core.OriginProcess,
}

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
// correlation_ref, title, actor and lifecycle each become a first-class event field, and
// each is REMOVED from attributes so one value is not stored twice under two names. An
// unrecognised key stays in attributes untouched.
//
// The mapping previously had no assertions at all. Mutating any of the four pulls to a
// constant "", or deleting the delete(attrs, key) line, passed the entire suite.
func TestEventFromCloudEventLiftsEveryRecognisedDataKey(t *testing.T) {
	ce := ceWithData(t, map[string]any{
		"correlation_ref": "dep-7",
		"title":           "Deploy api",
		"actor":           "alice",
		"lifecycle":       "started",
		"region":          "eu-west-1",
	})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal)
	require.NoError(t, err)

	assert.Equal(t, "github/dep-7", ev.CorrelationRef.String(),
		"correlation_ref must be lifted out of data and prefixed with the locus")
	assert.Equal(t, "Deploy api", ev.Title, "title must be lifted out of data")
	assert.Equal(t, "alice", ev.Actor, "actor must be lifted out of data")
	assert.Equal(t, core.LifecycleStarted, ev.Lifecycle, "lifecycle must be lifted out of data")

	assert.Equal(t, map[string]any{"region": "eu-west-1"}, ev.Attributes,
		"a lifted key must not remain in attributes, and an unrecognised key must stay")
}

// Actor reaches the event only through the data payload: CloudEvents has no envelope
// attribute for it, and POST /v1/events carries it as a first-class request field. An
// actorless CloudEvent is normal rather than invalid, so it yields an empty actor and
// no error.
func TestEventFromCloudEventWithoutActorIsValid(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal)
	require.NoError(t, err)
	assert.Empty(t, ev.Actor, "an event carrying no actor is accepted with an empty one")
}

// A recognised key holding a non-string value is left in attributes rather than coerced.
// Stringifying it would put a number into a field the schema promises is a title, and
// silently remove the caller's own attribute in the process.
func TestEventFromCloudEventLeavesNonStringRecognisedKeysAsAttributes(t *testing.T) {
	ce := ceWithData(t, map[string]any{"title": 7, "actor": true, "lifecycle": 7})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal)
	require.NoError(t, err)

	assert.Empty(t, ev.Title, "a non-string title must not be coerced into the field")
	assert.Empty(t, ev.Actor, "a non-string actor must not be coerced into the field")
	assert.Empty(t, ev.Lifecycle, "a non-string lifecycle must not be coerced into the field")
	assert.Equal(t,
		map[string]any{"title": float64(7), "actor": true, "lifecycle": float64(7)},
		ev.Attributes,
		"a value the field cannot hold must survive as the attribute it was sent as")
}

// TestEventFromCloudEventTakesTenantFromThePrincipal pins that tenant is never read from
// the envelope or its data, whether the CloudEvent carries one or not: it always comes
// from the resolved credential.
func TestEventFromCloudEventTakesTenantFromThePrincipal(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal)
	require.NoError(t, err)
	assert.Equal(t, testPrincipal.Tenant.String(), ev.Tenant)
}

// TestEventFromCloudEventRejectsDataCarryingTenant mirrors POST /v1/events's rule for
// the CloudEvents path: silently dropping a field that used to be authoritative is how a
// broken migration ships unnoticed, so a CloudEvent still asserting a tenant is refused
// rather than ignored, whatever type the value is.
func TestEventFromCloudEventRejectsDataCarryingTenant(t *testing.T) {
	for name, tenant := range map[string]any{"string": "other", "number": 7, "null": nil} {
		t.Run(name, func(t *testing.T) {
			ce := ceWithData(t, map[string]any{"tenant": tenant, "correlation_ref": "dep-7"})
			_, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal)
			require.Error(t, err)
			assert.ErrorIs(t, err, core.ErrInvalidEvent)
			assert.Contains(t, err.Error(), "tenant is derived from the credential")
		})
	}
}

// TestEventFromCloudEventSetsOriginFromThePrincipal pins that origin, like tenant, comes
// from the resolved credential rather than from anything in the envelope: a caller-
// asserted origin would be worthless, since CloudEvents data is otherwise caller-chosen.
func TestEventFromCloudEventSetsOriginFromThePrincipal(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal)
	require.NoError(t, err)
	assert.Equal(t, core.OriginProcess, ev.Origin)
}

// TestEventFromCloudEventSetsLocusFromThePrincipal pins that locus, like tenant and
// origin, comes from the resolved credential rather than from anything in the envelope:
// core.Event's own doc comment makes the same argument for Locus that it makes for
// Origin, since RefFor composes a point change's ref from it.
func TestEventFromCloudEventSetsLocusFromThePrincipal(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal)
	require.NoError(t, err)
	assert.Equal(t, testPrincipal.Locus, ev.Locus.String())
}

// TestEventFromCloudEventRejectsARefOverTheCap pins that the CloudEvents path validates
// the stored, locus-prefixed ref through the same rule POST /v1/events does, rather than
// validating the caller's shorter, unprefixed one. It must wrap core.ErrInvalidEvent:
// postCloudEvent answers every error from here 400 today regardless, but an unwrapped
// error would report an over-cap ref as a retryable 503 to any future caller that routes
// it through appendFailed instead.
func TestEventFromCloudEventRejectsARefOverTheCap(t *testing.T) {
	ce := ceWithData(t, map[string]any{
		"correlation_ref": strings.Repeat("x", auth.MaxRefBytes-len("github/")+1),
	})

	_, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidEvent)
	assert.Contains(t, err.Error(), "ref")
}

// TestEventFromCloudEventRejectsARefWithLeadingWhitespaceEvenAfterTheLocusPrefix pins
// that the whitespace rule is checked on the producer's ref BEFORE it is composed with
// the locus, not after. Composing first moves a leading space away from the string's
// edge — "github/ dep-1" is not caught by the same check that rejects " dep-1" — which
// would silently admit the one producer mistake that check exists to catch: an untrimmed
// ref posting a start under one ref and its completion under another.
func TestEventFromCloudEventRejectsARefWithLeadingWhitespaceEvenAfterTheLocusPrefix(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": " dep-1"})

	_, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidEvent)
	assert.Contains(t, err.Error(), "whitespace")
}

// An absent tenant in data is not an error: only a PRESENT one is, per
// TestEventFromCloudEventRejectsDataCarryingTenant. This is the other half of that rule.
func TestEventFromCloudEventDataOmittingTenantIsAccepted(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	_, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal)
	assert.NoError(t, err)
}

// The CloudEvents time attribute is OPTIONAL, so an envelope without one must still
// produce a usable occurred_at. Receipt time is the honest fallback: it is the earliest
// moment the system can attest to. Without the fallback the event carries a zero time,
// and a zero occurred_at drags first_seen_at — a running minimum — to year 1 for the
// whole ref.
func TestEventFromCloudEventFallsBackToReceiptTimeWithoutAnEnvelopeTime(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})
	ce.SetTime(time.Time{})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal)
	require.NoError(t, err)

	assert.Equal(t, ceNow, ev.OccurredAt,
		"an envelope with no time must fall back to receipt time, not to the zero time")
	assert.False(t, ev.OccurredAt.IsZero(), "a zero occurred_at would poison first_seen_at")
}

// An envelope that DOES carry a time keeps it: the fallback must not overwrite a stated
// occurrence with receipt time, or every event would look like it happened on arrival.
func TestEventFromCloudEventKeepsAnEnvelopeTime(t *testing.T) {
	ce := ceWithData(t, map[string]any{"correlation_ref": "dep-7"})

	ev, err := eventFromCloudEvent(ce, ceNow, "01A", testPrincipal)
	require.NoError(t, err)
	assert.Equal(t, ceOccurred, ev.OccurredAt, "a stated occurrence time must be preserved")
}
