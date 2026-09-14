package core_test

import (
	"maps"
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
	t30 = time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)
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

func TestFoldTitleFallsBackToType(t *testing.T) {
	c := core.Fold(nil, ev("01A", "dep-7", "deployment.started", t10))
	assert.Equal(t, "deployment.started", c.Title)

	e := ev("01A", "dep-8", "deployment.started", t10)
	e.Title = "API v2.5.0"
	assert.Equal(t, "API v2.5.0", core.Fold(nil, e).Title)
}

// F1: TestFoldIsPure must actually be capable of failing. base carries a
// non-empty attribute map with a key the second event overwrites, so an
// implementation that shares (rather than clones) cur's attribute map is
// caught by comparing a pre-fold clone against base.Attributes afterwards.
// x and y are compared as whole structs, not field-by-field, so a divergence
// in any of Change's eleven fields fails the test.
func TestFoldIsPure(t *testing.T) {
	a := ev("01A", "dep-7", "deployment.started", t10)
	a.Attributes = map[string]any{"service.version": "2.4.0"}
	base := core.Fold(nil, a)
	baseAttrsBefore := maps.Clone(base.Attributes)

	b := ev("01B", "dep-7", "deployment.completed", t20)
	b.Attributes = map[string]any{"service.version": "2.5.0"}

	x := core.Fold(&base, b)
	y := core.Fold(&base, b)

	assert.Equal(t, x, y, "folding identical input twice must give identical output")
	assert.Equal(t, baseAttrsBefore, base.Attributes,
		"Fold must not mutate the input change's attribute map")
}

// F2: a terminal event folded onto a change that already has a non-nil End
// must assign a fresh *time.Time, not write through the copied pointer. If it
// wrote through (*out.End = ev.OccurredAt), the earlier change's End would be
// mutated in place, since `out = *cur` copies the pointer value, not the
// pointee.
func TestFoldTerminalEventReplacesEndWithFreshPointer(t *testing.T) {
	c := core.Fold(nil, ev("01A", "dep-7", "deployment.started", t10))
	c = core.Fold(&c, ev("01B", "dep-7", "deployment.completed", t20))
	require.NotNil(t, c.End)
	firstEnd := c.End

	next := core.Fold(&c, ev("01C", "dep-7", "deployment.rolled_back", t30))

	require.NotNil(t, next.End)
	assert.Equal(t, t30, *next.End)
	assert.Equal(t, t20, *firstEnd,
		"the earlier change's End must not be mutated through the shared pointer")
	assert.Equal(t, t20, *c.End, "the prior change value must remain untouched")
}

// F3: Fold(nil, ev) and Fold(&Change{}, ev) must converge on identical
// output for the same event sequence. Task 8 rebuilds the projection with
// `c := m[ref]; m[ref] = Fold(&c, ev)` over a map[string]Change, which passes
// a zero-value *Change rather than nil for the first event of a ref.
func TestFoldNilAndZeroValueCurConverge(t *testing.T) {
	events := []core.Event{
		ev("01A", "", "config.changed", t10),            // point
		ev("01B", "dep-7", "deployment.started", t10),   // open change
		ev("01C", "dep-8", "deployment.started", t10),   // closed change, part 1
		ev("01D", "dep-8", "deployment.completed", t20), // closed change, part 2
	}

	nilFirst := map[string]core.Change{}
	seen := map[string]bool{}
	for _, e := range events {
		ref := core.RefFor(e)
		if !seen[ref] {
			nilFirst[ref] = core.Fold(nil, e)
			seen[ref] = true
			continue
		}
		cur := nilFirst[ref]
		nilFirst[ref] = core.Fold(&cur, e)
	}

	zeroValueFirst := map[string]core.Change{}
	for _, e := range events {
		ref := core.RefFor(e)
		cur := zeroValueFirst[ref]
		zeroValueFirst[ref] = core.Fold(&cur, e)
	}

	assert.Equal(t, nilFirst, zeroValueFirst,
		"Fold(nil, ev) and Fold(&Change{}, ev) must produce identical projections")
}

// F4: a .started and a .completed at the same timestamp is a zero-duration
// closed interval, not a point. Only the no-correlation-ref path produces a
// point.
func TestFoldZeroDurationClosedIntervalIsNotAPoint(t *testing.T) {
	c := core.Fold(nil, ev("01A", "dep-7", "deployment.started", t10))
	c = core.Fold(&c, ev("01B", "dep-7", "deployment.completed", t10))

	require.NotNil(t, c.End)
	assert.True(t, c.End.Equal(c.Start), "precondition: start and end coincide")
	assert.False(t, c.IsPoint(), "a same-timestamp start/complete is a closed interval, not a point")
	assert.False(t, c.IsOpen())
}

func TestRefFor(t *testing.T) {
	withCorr := ev("01A", "dep-7", "deployment.started", t10)
	assert.Equal(t, "dep-7", core.RefFor(withCorr))

	withoutCorr := ev("01B", "", "config.changed", t10)
	assert.Equal(t, "01B", core.RefFor(withoutCorr), "a point's ref is its own event id")
}

// F5: pins the full v0.1 terminal vocabulary, including the additions for
// F5 (resolved/closed/done/ended -> completed, aborted -> cancelled,
// errored/timed_out -> failed), plus non-terminal types and a three-segment
// type to exercise the last-dot-segment rule.
func TestStatusForTypeTerminalVocabulary(t *testing.T) {
	tests := []struct {
		typ      string
		status   core.Status
		terminal bool
	}{
		{"deployment.completed", core.StatusCompleted, true},
		{"deployment.finished", core.StatusCompleted, true},
		{"deployment.succeeded", core.StatusCompleted, true},
		{"incident.resolved", core.StatusCompleted, true},
		{"incident.closed", core.StatusCompleted, true},
		{"task.done", core.StatusCompleted, true},
		{"maintenance.ended", core.StatusCompleted, true},
		{"deployment.failed", core.StatusFailed, true},
		{"job.errored", core.StatusFailed, true},
		{"job.timed_out", core.StatusFailed, true},
		{"deployment.rolled_back", core.StatusRolledBack, true},
		{"deployment.rolledback", core.StatusRolledBack, true},
		{"deployment.cancelled", core.StatusCancelled, true},
		{"deployment.canceled", core.StatusCancelled, true},
		{"incident.aborted", core.StatusCancelled, true},
		{"deployment.started", "", false},
		{"deployment.begun", "", false},
		{"config.changed", "", false},
		{"deployment", "", false},
		{"ci.deployment.failed", core.StatusFailed, true},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			status, terminal := core.StatusForType(tt.typ)
			assert.Equal(t, tt.terminal, terminal)
			assert.Equal(t, tt.status, status)
		})
	}
}
