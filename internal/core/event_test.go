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

func TestValidateRejectsMissingFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*core.Event)
		want   string
	}{
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
