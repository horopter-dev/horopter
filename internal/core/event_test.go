package core_test

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/core"
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
		CorrelationRef: core.RefFromRow("dep-7"),
		Attributes:     map[string]any{"service.version": "2.5.0"},
		Origin:         core.OriginProcess,
		Locus:          core.LocusFromRow("buildkite"),
	}
}

func TestValidateAcceptsValidEvent(t *testing.T) {
	require.NoError(t, validEvent().Validate())
}

// TestValidateAcceptsEveryLifecycle pins core.Lifecycles to what Validate admits.
func TestValidateAcceptsEveryLifecycle(t *testing.T) {
	for _, lc := range core.Lifecycles {
		t.Run(string(lc), func(t *testing.T) {
			e := validEvent()
			e.Lifecycle = lc
			require.NoError(t, e.Validate())
		})
	}
}

// TestValidateRejectsMissingFields checks each failure names its field; "invalid" contains "id".
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
		{"no origin", func(e *core.Event) { e.Origin = "" }, "origin"},
		{"unknown origin", func(e *core.Event) { e.Origin = "robot" }, "origin"},
		{"wrong-case lifecycle", func(e *core.Event) { e.Lifecycle = "Started" }, "Started"},
		{"abbreviated lifecycle", func(e *core.Event) { e.Lifecycle = "start" }, "start"},
		{"lifecycle from the wrong vocabulary",
			func(e *core.Event) { e.Lifecycle = "occurred" }, "occurred"},
		{"synonym the type suffix accepts",
			func(e *core.Event) { e.Lifecycle = "finished" }, "finished"},
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
	//nolint:gosec // G404: a fixed seed is what makes this determinism test deterministic.
	src := rand.New(rand.NewSource(1))
	early := core.NewULID(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), src)
	late := core.NewULID(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), src)
	assert.Less(t, early, late, "ULIDs must sort lexicographically by time")
	assert.Len(t, early, 26)
}

// TestValidateFailuresAreInvalidEvent: an unwrapped failure would be answered 503 and retried.
func TestValidateFailuresAreInvalidEvent(t *testing.T) {
	valid := core.Event{
		ID:     "01J0000000000000000000000A",
		Tenant: "default", SchemaVersion: core.CurrentSchemaVersion,
		Source: "ci", Type: "deployment.started", Subject: "acme/prod",
		OccurredAt: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
		Origin:     core.OriginProcess,
		Locus:      core.LocusFromRow("github"),
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
		"origin":         {func(e *core.Event) { e.Origin = "" }, "origin"},
		"locus_empty":    {func(e *core.Event) { e.Locus = core.Locus{} }, "locus"},
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

// TestCheckAttributeSize pins the cap's measure; each size is counted by hand from its JSON.
func TestCheckAttributeSize(t *testing.T) {
	for _, tc := range []struct {
		name  string
		attrs map[string]any
		limit int
		want  string
	}{
		// {"k":"xxxx"} is 1+3+1+6+1 = 12 bytes.
		{"exactly the limit", map[string]any{"k": "xxxx"}, 12, ""},
		{"one byte over", map[string]any{"k": "xxxx"}, 11,
			"attributes encode to 12 bytes of JSON; the limit is 11"},
		// A nil map is stored as {}: 2 bytes. Encoded as-is it would be null, 4 bytes.
		{"nil map at its size", nil, 2, ""},
		{"nil map over", nil, 1, "attributes encode to 2 bytes of JSON; the limit is 1"},
		// {"a":{"b":[1,2]}} is 1+3+1+1+3+1+5+1+1 = 17 bytes.
		{"nested values count", map[string]any{"a": map[string]any{"b": []any{1, 2}}}, 16,
			"attributes encode to 17 bytes of JSON; the limit is 16"},
		// {"t":"a&b"} is 1+3+1+5+1 = 11 bytes; HTML escaping would make & six bytes.
		{"ampersand counts once", map[string]any{"t": "a&b"}, 11, ""},
		{"unencodable value", map[string]any{"x": math.Inf(1)}, 1024,
			"attributes cannot be encoded as JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent()
			e.Attributes = tc.attrs
			err := e.CheckAttributeSize(tc.limit)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, core.ErrInvalidEvent)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestCheckAttributeSizeTellsTheProducerWhatToDo pins the whole message the producer reads.
func TestCheckAttributeSizeTellsTheProducerWhatToDo(t *testing.T) {
	e := validEvent()
	e.Attributes = map[string]any{"k": "xxxx"}

	err := e.CheckAttributeSize(11)

	require.Error(t, err)
	assert.Equal(t, `invalid event "01J0000000000000000000000A": attributes encode to 12 `+
		`bytes of JSON; the limit is 11 — keep attributes to identifiers and short text, `+
		`and link to anything larger`, err.Error())
}
