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
	assert.Equal(t, core.StatusOccurred, c.Status, "only the no-correlation-ref path sets it")
	require.NotNil(t, c.End)
	assert.True(t, c.End.Equal(c.Start), "a point's end coincides with its start")
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

// TestFoldIsPure must actually be capable of failing. base carries a
// non-empty attribute map with a key the second event overwrites, so an
// implementation that shares (rather than clones) cur's attribute map is
// caught by comparing a pre-fold clone against base.Attributes afterwards.
// x and y are compared as whole structs, not field-by-field, so a divergence
// in any of Change's fields fails the test.
//
// The second event is a start-type carrying an EARLIER occurred_at, which is
// the one input that makes foldLifecycle write StartedAt on a change that
// already has one. out.StartedAt is a pointer copied wholesale from cur, so an
// implementation that wrote through it (*out.StartedAt = ev.OccurredAt)
// instead of assigning a fresh &at would reach back into the caller's Change
// and silently rewrite it. Comparing x against y cannot catch that — both
// would hold the same mutated pointer — so base.StartedAt is captured by VALUE
// beforehand and asserted afterwards. With a terminal second event, as this
// test used to have, that branch is never entered and the pointer is never
// exercised at all.
func TestFoldIsPure(t *testing.T) {
	a := ev("01A", "dep-7", "deployment.started", t20)
	a.Attributes = map[string]any{"service.version": "2.4.0"}
	base := core.Fold(nil, a)
	baseAttrsBefore := maps.Clone(base.Attributes)
	require.NotNil(t, base.StartedAt, "precondition: base carries an asserted start")
	baseStartedBefore := *base.StartedAt

	b := ev("01B", "dep-7", "deployment.started", t10)
	b.Attributes = map[string]any{"service.version": "2.5.0"}

	x := core.Fold(&base, b)
	y := core.Fold(&base, b)

	assert.Equal(t, x, y, "folding identical input twice must give identical output")
	assert.Equal(t, baseAttrsBefore, base.Attributes,
		"Fold must not mutate the input change's attribute map")
	require.NotNil(t, base.StartedAt)
	assert.Equal(t, baseStartedBefore, *base.StartedAt,
		"Fold must not write through the StartedAt pointer it copied from cur; the "+
			"caller's Change must be untouched")
	assert.Equal(t, t10, *x.StartedAt,
		"precondition: the fold really did take the earlier assertion, so the branch "+
			"that writes StartedAt was entered")
}

// A terminal event folded onto a change that already has a non-nil End
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

// Fold(nil, ev) and Fold(&Change{}, ev) must converge on identical output for the
// same event sequence. Accumulating into a map[string]Change invites the spelling
// `c := m[ref]; m[ref] = Fold(&c, ev)`, which passes a zero-value *Change rather than
// nil for the first event of a ref; foldEventLog in internal/store/postgres passes nil.
// Both must agree, or a rebuild and the incremental append path would disagree about
// every ref's first event.
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

// A .started and a .completed at the same timestamp is a zero-duration
// closed interval, not a point. Only the no-correlation-ref path produces a
// point.
func TestFoldZeroDurationClosedIntervalIsNotAPoint(t *testing.T) {
	c := core.Fold(nil, ev("01A", "dep-7", "deployment.started", t10))
	c = core.Fold(&c, ev("01B", "dep-7", "deployment.completed", t10))

	require.NotNil(t, c.End)
	assert.True(t, c.End.Equal(c.Start), "precondition: start and end coincide")
	assert.NotEqual(t, core.StatusOccurred, c.Status,
		"a same-timestamp start/complete is a closed interval, not a point — "+
			"status is what separates them, since End == Start holds for both")
	assert.False(t, c.IsOpen())
}

func TestRefFor(t *testing.T) {
	withCorr := ev("01A", "dep-7", "deployment.started", t10)
	assert.Equal(t, "dep-7", core.RefFor(withCorr))

	withoutCorr := ev("01B", "", "config.changed", t10)
	assert.Equal(t, "01B", core.RefFor(withoutCorr), "a point's ref is its own event id")
}

// Pins the full v0.1 terminal vocabulary, including resolved/closed/done/ended
// -> completed, aborted -> cancelled and errored/timed_out -> failed, plus
// non-terminal types and a three-segment type to exercise the last-dot-segment
// rule.
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

// A correlated event that is neither a start nor a terminal type must still leave a
// queryable status. Before this, neither branch fired and Status stayed "", which wrote
// a row QueryChanges could not select (it reads an empty Status as "no filter") and
// which, with End nil, rendered as an interval growing to the year 9999. A change has
// three shapes and admits no fourth, so such an event means "in flight".
func TestFoldCorrelatedNonTerminalIsInProgress(t *testing.T) {
	for _, typ := range []string{
		"incident.declared",
		"deployment.triggered",
		"maintenance.scheduled",
		"deployment.annotated",
	} {
		t.Run(typ, func(t *testing.T) {
			c := core.Fold(nil, ev("01A", "dep-7", typ, t10))
			assert.Equal(t, core.StatusInProgress, c.Status,
				"a correlated non-terminal event must leave a queryable status")
			assert.True(t, c.IsOpen(), "it ends nothing, so the change stays open")
			assert.Equal(t, t10, c.Start)
		})
	}
}

// The in-progress default must not reopen a change that already reached a terminal
// status. A late annotation on a finished deployment is ordinary traffic, and rewriting
// its outcome to in_progress while End stays set would be a worse corruption than the
// empty status it replaced.
func TestFoldNonTerminalAfterTerminalKeepsTheOutcome(t *testing.T) {
	c := core.Fold(nil, ev("01A", "dep-7", "deployment.started", t10))
	c = core.Fold(&c, ev("01B", "dep-7", "deployment.completed", t20))
	c = core.Fold(&c, ev("01C", "dep-7", "deployment.annotated", t30))

	assert.Equal(t, core.StatusCompleted, c.Status,
		"a later annotation must not reopen a completed change")
	require.NotNil(t, c.End)
	assert.Equal(t, t20, *c.End, "the recorded end must survive the annotation")
}

// Start falls back to the EARLIEST occurred_at seen for the ref, not the first one. The
// implementation used to keep the FIRST FOLDED one, which differs precisely when a
// later non-start, non-terminal event carries an earlier timestamp — the case below.
func TestFoldStartFallsBackToTheEarliestOccurredAt(t *testing.T) {
	c := core.Fold(nil, ev("01B", "dep-7", "deployment.completed", t20))
	require.Equal(t, t20, c.Start, "precondition: the only event so far sets the fallback")

	c = core.Fold(&c, ev("01A", "dep-7", "deployment.annotated", t10))
	assert.Equal(t, t10, c.Start,
		"a later event with an earlier occurred_at must lower the fallback start")
	require.NotNil(t, c.End)
	assert.Equal(t, t20, *c.End, "the recorded end must survive")

	// A real start still wins outright, even when it is later than the fallback.
	c = core.Fold(&c, ev("01C", "dep-7", "deployment.started", t30))
	assert.Equal(t, t30, c.Start, "the real start must overwrite the fallback, not be min'd with it")
}

// The case the two-fact start model was introduced to fix: a stray annotation carrying a
// timestamp EARLIER than the change's declared start used to drag the recorded start
// backwards behind the event that authoritatively declared it — silently, and only in one
// of the two arrival orders. The assertion must win, and the earlier evidence must still
// be recorded rather than destroyed.
func TestFoldAnAssertedStartSurvivesALaterEventCarryingAnEarlierTime(t *testing.T) {
	c := core.Fold(nil, ev("01A", "dep-7", "deployment.started", t20))
	require.NotNil(t, c.StartedAt)
	require.Equal(t, t20, *c.StartedAt, "precondition: the start event asserts t20")

	c = core.Fold(&c, ev("01B", "dep-7", "deployment.annotated", t10))

	assert.Equal(t, t20, c.Start,
		"an annotation cannot move a declared start backwards; the assertion wins")
	require.NotNil(t, c.StartedAt)
	assert.Equal(t, t20, *c.StartedAt)
	assert.Equal(t, t10, c.FirstSeenAt,
		"the earlier evidence must still be recorded — first_seen_at < started_at is the "+
			"anomaly worth auditing, and a single start field destroyed it")
}

// A ref carrying more than one start-type event takes the EARLIEST assertion. The
// decision document reasons from the assertion being idempotent, which holds for one
// .started and fails for two with different timestamps: last-folded-wins would make a
// rebuild (id order) and a live append (arrival order) disagree. Minimum is commutative,
// so taking it is what keeps the document's stated property true.
func TestFoldEarliestStartAssertionWins(t *testing.T) {
	later := core.Fold(nil, ev("01A", "dep-7", "deployment.started", t30))
	later = core.Fold(&later, ev("01B", "dep-7", "deployment.started", t10))

	earlier := core.Fold(nil, ev("01B", "dep-7", "deployment.started", t10))
	earlier = core.Fold(&earlier, ev("01A", "dep-7", "deployment.started", t30))

	require.NotNil(t, later.StartedAt)
	require.NotNil(t, earlier.StartedAt)
	assert.Equal(t, t10, *later.StartedAt, "the later-arriving earlier assertion must win")
	assert.Equal(t, t10, *earlier.StartedAt)
	assert.Equal(t, later.Start, earlier.Start,
		"two start-type events must fold to the same Start in either order")
}

// permutations returns every ordering of events, by recursive swap.
func permutations(events []core.Event) [][]core.Event {
	if len(events) <= 1 {
		return [][]core.Event{append([]core.Event(nil), events...)}
	}
	var out [][]core.Event
	for i := range events {
		rest := make([]core.Event, 0, len(events)-1)
		rest = append(rest, events[:i]...)
		rest = append(rest, events[i+1:]...)
		for _, tail := range permutations(rest) {
			out = append(out, append([]core.Event{events[i]}, tail...))
		}
	}
	return out
}

// Folding the same set of events in ANY order must produce the same StartedAt,
// FirstSeenAt and Start. This is the property that makes RebuildProjection meaningful —
// a rebuild replays in id order while a live append folds in arrival order, and the two
// must converge — and it is the property the decision document claims. Both facts are
// minima, which is what makes it hold; a rule that let the last-folded start win would
// fail here on the two .started events alone.
//
// Only the three start fields are asserted. Title, Type, Actor, Subject, Status, End,
// Attributes and LastEventID are genuinely last-folded-wins and are expected to differ
// between orderings; asserting the whole struct would fail for reasons this test is not
// about.
func TestFoldStartFactsAreInsensitiveToFoldOrder(t *testing.T) {
	t40 := time.Date(2026, 9, 14, 10, 40, 0, 0, time.UTC)
	events := []core.Event{
		ev("01A", "dep-7", "deployment.started", t20),
		ev("01B", "dep-7", "deployment.annotated", t10),
		ev("01C", "dep-7", "deployment.started", t30),
		ev("01D", "dep-7", "deployment.completed", t40),
	}

	orders := permutations(events)
	require.Len(t, orders, 24, "all 4! orderings must be exercised")

	for _, order := range orders {
		ids := make([]string, 0, len(order))
		for _, e := range order {
			ids = append(ids, e.ID)
		}
		var c core.Change
		for i, e := range order {
			if i == 0 {
				c = core.Fold(nil, e)
				continue
			}
			c = core.Fold(&c, e)
		}
		require.NotNil(t, c.StartedAt, "order %v: an asserted start must survive", ids)
		assert.Equal(t, t20, *c.StartedAt,
			"order %v: the earliest of the two assertions must win in every order", ids)
		assert.Equal(t, t10, c.FirstSeenAt,
			"order %v: the earliest occurred_at seen must win in every order", ids)
		assert.Equal(t, t20, c.Start,
			"order %v: Start is COALESCE(StartedAt, FirstSeenAt) and must not depend on "+
				"fold order — a rebuild replays in id order, a live append in arrival order", ids)
	}
}
