package core_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/core"
)

func ev(id, corr, typ string, at time.Time) core.Event {
	return core.Event{
		ID: id, Tenant: "default", SchemaVersion: 1,
		Source: "ci", Type: typ, Subject: "acme/prod", Actor: "ci-bot",
		OccurredAt: at, CorrelationRef: corr,
		Attributes: map[string]any{},
	}
}

var (
	t10 = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	t20 = time.Date(2026, 9, 14, 10, 20, 0, 0, time.UTC)
)

func TestFoldStartedProducesOpenChange(t *testing.T) {
	c := core.Fold(nil, ev("01A", "dep-7", "deployment.started", t10))
	assert.Equal(t, "dep-7", c.Ref)
	assert.Equal(t, t10, c.Start)
	assert.True(t, c.IsOpen())
	assert.Equal(t, core.StatusInProgress, c.Status)
}

func TestFoldFinishedClosesChange(t *testing.T) {
	c := core.Fold(nil, ev("01A", "dep-7", "deployment.started", t10))
	c = core.Fold(&c, ev("01B", "dep-7", "deployment.failed", t20))
	require.NotNil(t, c.End)
	assert.Equal(t, t20, *c.End)
	assert.Equal(t, core.StatusFailed, c.Status)
	assert.Equal(t, "01B", c.LastEventID)
}

func TestFoldEventWithoutCorrelationRefIsAPoint(t *testing.T) {
	c := core.Fold(nil, ev("01C", "", "config.changed", t10))
	assert.Equal(t, "01C", c.Ref, "a point's ref is its own event id")
	assert.True(t, c.IsPoint())
	assert.Equal(t, core.StatusOccurred, c.Status)
}

func TestFoldOutOfOrderFinishBeforeStart(t *testing.T) {
	// The terminal event arrives first. Start must fall back to the earliest
	// occurred_at seen, then be corrected when the real start arrives.
	c := core.Fold(nil, ev("01B", "dep-7", "deployment.completed", t20))
	assert.Equal(t, t20, c.Start, "a change must never exist without a start")
	assert.Equal(t, core.StatusCompleted, c.Status)

	c = core.Fold(&c, ev("01A", "dep-7", "deployment.started", t10))
	assert.Equal(t, t10, c.Start, "the real start must overwrite the fallback")
	require.NotNil(t, c.End)
	assert.Equal(t, t20, *c.End, "the recorded end must survive")
}

func TestFoldMergesAttributesLaterWins(t *testing.T) {
	a := ev("01A", "dep-7", "deployment.started", t10)
	a.Attributes = map[string]any{"service.version": "2.4.0", "cicd.pipeline.name": "api"}
	b := ev("01B", "dep-7", "deployment.completed", t20)
	b.Attributes = map[string]any{"service.version": "2.5.0"}

	c := core.Fold(nil, a)
	c = core.Fold(&c, b)
	assert.Equal(t, "2.5.0", c.Attributes["service.version"])
	assert.Equal(t, "api", c.Attributes["cicd.pipeline.name"])
}

func TestFoldIsPure(t *testing.T) {
	// Folding the same input twice must give identical output, and must not
	// mutate the input change or its attribute map.
	base := core.Fold(nil, ev("01A", "dep-7", "deployment.started", t10))
	snapshot := base
	snapshotAttrs := len(base.Attributes)

	x := core.Fold(&base, ev("01B", "dep-7", "deployment.completed", t20))
	y := core.Fold(&base, ev("01B", "dep-7", "deployment.completed", t20))

	assert.Equal(t, x.Status, y.Status)
	assert.Equal(t, x.Start, y.Start)
	assert.Equal(t, snapshot.Status, base.Status, "Fold must not mutate its input")
	assert.Len(t, base.Attributes, snapshotAttrs, "Fold must not mutate the input attribute map")
}

func TestFoldTitleFallsBackToType(t *testing.T) {
	c := core.Fold(nil, ev("01A", "dep-7", "deployment.started", t10))
	assert.Equal(t, "deployment.started", c.Title)

	e := ev("01A", "dep-8", "deployment.started", t10)
	e.Title = "API v2.5.0"
	assert.Equal(t, "API v2.5.0", core.Fold(nil, e).Title)
}
