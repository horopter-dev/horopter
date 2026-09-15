package core_test

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/core"
)

func validEvent() core.Event {
	return core.Event{
		ID:             "01J0000000000000000000000A",
		Tenant:         "default",
		IdempotencyKey: "build-42",
		SchemaVersion:  core.CurrentSchemaVersion,
		Source:         "buildkite/pipelines/api",
		Type:           "deployment.started",
		Subject:        "acme/prod",
		Actor:          "ci-bot",
		OccurredAt:     time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
		ReceivedAt:     time.Date(2026, 9, 14, 10, 0, 1, 0, time.UTC),
		CorrelationRef: "dep-7",
		Attributes:     map[string]any{"service.version": "2.5.0"},
	}
}

func TestValidateAcceptsValidEvent(t *testing.T) {
	require.NoError(t, validEvent().Validate())
}

// TestValidateRejectsMissingFields checks that each required field's failure names that
// field back to the caller.
//
// want is the fragment the field's own branch produces, and for the id it is NOT the
// word "id". "id" appears in ErrInvalidEvent's own text — inval·id — and in the
// "not-a-ulid" fixture, so asserting on it passes whatever Validate reports and whichever
// branch produced it. "26-character ULID" appears only in the id branch.
func TestValidateRejectsMissingFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*core.Event)
		want   string
	}{
		{"malformed id", func(e *core.Event) { e.ID = "not-a-ulid" }, "26-character ULID"},
		{"no tenant", func(e *core.Event) { e.Tenant = "" }, "tenant"},
		{"no type", func(e *core.Event) { e.Type = "" }, "type"},
		{"no subject", func(e *core.Event) { e.Subject = "" }, "subject"},
		{"no source", func(e *core.Event) { e.Source = "" }, "source"},
		{"zero occurred_at", func(e *core.Event) { e.OccurredAt = time.Time{} }, "occurred_at"},
		{"unknown schema", func(e *core.Event) { e.SchemaVersion = 99 }, "schema_version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent()
			tc.mutate(&e)
			err := e.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestNewULIDIsSortableByTime(t *testing.T) {
	//nolint:gosec // G404: the fixed seed is what makes this determinism test deterministic;
	// crypto/rand would defeat the assertion. Test-only, never reached by production code.
	src := rand.New(rand.NewSource(1))
	early := core.NewULID(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), src)
	late := core.NewULID(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), src)
	assert.Less(t, early, late, "ULIDs must sort lexicographically by time")
	assert.Len(t, early, 26)
}

// TestValidateFailuresAreInvalidEvent pins the taxonomy every caller branches on. A
// validation failure that forgets to wrap ErrInvalidEvent is not a compile error and
// changes no message, so nothing else would notice — but ingress would silently start
// answering it 503 and telling the producer to retry a body that can never be accepted.
// Each case names a different required field so a single missing wrap is caught rather
// than masked by the others.
//
// wantIn is carried separately from the subtest name because for the id they differ.
// Asserting the message contains "id" asserts nothing: ErrInvalidEvent's own text is
// "invalid event" — inval·id — and the fixture is "not-a-ulid", so that assertion holds
// whichever branch fired and whatever it said. "26-character ULID" is produced by the id
// branch alone.
func TestValidateFailuresAreInvalidEvent(t *testing.T) {
	valid := core.Event{
		ID:     "01J0000000000000000000000A",
		Tenant: "default", SchemaVersion: core.CurrentSchemaVersion,
		Source: "ci", Type: "deployment.started", Subject: "acme/prod",
		OccurredAt: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
	}
	require.NoError(t, valid.Validate(), "precondition: the base event is acceptable")

	for name, tc := range map[string]struct {
		breakIt func(e *core.Event)
		wantIn  string
	}{
		"id":             {func(e *core.Event) { e.ID = "not-a-ulid" }, "26-character ULID"},
		"tenant":         {func(e *core.Event) { e.Tenant = "" }, "tenant"},
		"source":         {func(e *core.Event) { e.Source = "" }, "source"},
		"type":           {func(e *core.Event) { e.Type = "" }, "type"},
		"subject":        {func(e *core.Event) { e.Subject = "" }, "subject"},
		"occurred_at":    {func(e *core.Event) { e.OccurredAt = time.Time{} }, "occurred_at"},
		"schema_version": {func(e *core.Event) { e.SchemaVersion = 99 }, "schema_version"},
	} {
		t.Run(name, func(t *testing.T) {
			bad := valid
			tc.breakIt(&bad)
			err := bad.Validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, core.ErrInvalidEvent,
				"a %s failure must wrap ErrInvalidEvent; without it ingress answers 503 "+
					"and the producer retries a request that can never succeed", name)
			assert.Contains(t, err.Error(), tc.wantIn,
				"the message must still name the field the caller got wrong")
		})
	}
}
