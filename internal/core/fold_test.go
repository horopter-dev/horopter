package core_test

import (
	"crypto/rand"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/core"
)

// ev builds the event Fold sees, which is one read back out of the log — so its ref and
// locus are built the way a scan builds them, through the FromRow constructors, rather
// than through the composing ones a producer's input goes through.
func ev(id, corr, typ string, at time.Time) core.Event {
	return core.Event{
		ID: id, Tenant: "default", SchemaVersion: 1,
		Source: "ci", Type: typ, Subject: "acme/prod", Actor: "ci-bot",
		OccurredAt: at, CorrelationRef: core.RefFromRow(corr),
		Attributes: map[string]any{}, Locus: core.LocusFromRow("ci"),
	}
}

var (
	t10 = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	t20 = time.Date(2026, 9, 14, 10, 20, 0, 0, time.UTC)
	t30 = time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)
)

func TestFoldStartedProducesOpenChange(t *testing.T) {
	c := core.Fold(nil, ev("01A", "dep-7", "deployment.started", t10))
	assert.Equal(t, "dep-7", c.Ref.String())
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
	assert.Equal(t, "ci/01C", c.Ref.String(), "a point's ref is its locus and its own event id")
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
	baseSourcesBefore := append([]string{}, base.Sources...)
	require.NotNil(t, base.StartedAt, "precondition: base carries an asserted start")
	baseStartedBefore := *base.StartedAt

	b := ev("01B", "dep-7", "deployment.started", t10)
	b.Attributes = map[string]any{"service.version": "2.5.0"}
	b.Source = "flux"

	x := core.Fold(&base, b)
	y := core.Fold(&base, b)

	assert.Equal(t, x, y, "folding identical input twice must give identical output")
	assert.Equal(t, baseAttrsBefore, base.Attributes,
		"Fold must not mutate the input change's attribute map")
	assert.Equal(t, baseSourcesBefore, base.Sources,
		"Fold must not mutate the input change's sources slice; a's and b's sources "+
			"differ, so an implementation that aliased cur.Sources would be caught here")
	require.NotNil(t, base.StartedAt)
	assert.Equal(t, baseStartedBefore, *base.StartedAt,
		"Fold must not write through the StartedAt pointer it copied from cur; the "+
			"caller's Change must be untouched")
	assert.Equal(t, t10, *x.StartedAt,
		"precondition: the fold really did take the earlier assertion, so the branch "+
			"that writes StartedAt was entered")
	assert.Equal(t, []string{"ci", "flux"}, x.Sources,
		"precondition: the fold merged two distinct sources, which is what makes the "+
			"aliasing check above meaningful")
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
// same event sequence. Accumulating into a map[core.Ref]core.Change invites the spelling
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

	nilFirst := map[core.Ref]core.Change{}
	seen := map[core.Ref]bool{}
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

	zeroValueFirst := map[core.Ref]core.Change{}
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
	assert.Equal(t, "dep-7", core.RefFor(withCorr).String(),
		"a correlated ref is already locus-prefixed at ingest and passes through")

	withoutCorr := ev("01B", "", "config.changed", t10)
	assert.Equal(t, "ci/01B", core.RefFor(withoutCorr).String(),
		"a point's ref is its locus and its own event id, so there is one ref shape")
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
// two-fact start model reasons from the assertion being idempotent, which holds for one
// .started and fails for two with different timestamps: last-folded-wins would make a
// rebuild (id order) and a live append (arrival order) disagree. Minimum is commutative,
// so taking it is what keeps that property true.
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
// must converge — and it is the property the two-fact start model rests on. Both facts are
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

// A later event carrying no actor must not erase the one already recorded. Actor is
// optional on both ingest paths — omitted from a native request body, or absent from a
// CloudEvent's data payload — so an actorless follow-up event is ordinary traffic, and an
// unconditional assignment blanked change.actor as soon as one arrived, in the live and
// the rebuild path alike. Title carries the same hazard and is guarded against it in Fold.
//
// No existing test could catch this: the ev() helper gives every fixture the same
// non-empty actor, so last-wins and last-non-empty-wins are indistinguishable under it.
func TestFoldPreservesActorAgainstAnEventCarryingNone(t *testing.T) {
	named := ev("01A", "dep-7", "deployment.started", t10)
	named.Actor = "alice"
	anonymous := ev("01B", "dep-7", "deployment.annotated", t20)
	anonymous.Actor = ""

	c := core.Fold(nil, named)
	require.Equal(t, "alice", c.Actor, "precondition: the first event names the actor")
	c = core.Fold(&c, anonymous)
	assert.Equal(t, "alice", c.Actor,
		"an event carrying no actor must not erase the recorded one")

	// The unconditional assignment also made Actor depend on fold order: named-then-
	// anonymous yielded "", anonymous-then-named yielded "alice". A rebuild folds in id
	// order and a live append in arrival order, so the two could disagree about actor.
	rev := core.Fold(nil, anonymous)
	rev = core.Fold(&rev, named)
	assert.Equal(t, c.Actor, rev.Actor, "Actor must not depend on fold order")
}

// A later event that does name an actor still replaces the earlier one: the rule is
// last-non-empty-wins, not first-wins. Without this, a fix for the erasure above could
// freeze the first actor a ref ever saw and silently stop recording handovers.
func TestFoldLaterNonEmptyActorWins(t *testing.T) {
	first := ev("01A", "dep-7", "deployment.started", t10)
	first.Actor = "alice"
	second := ev("01B", "dep-7", "deployment.completed", t20)
	second.Actor = "bob"

	c := core.Fold(nil, first)
	c = core.Fold(&c, second)
	assert.Equal(t, "bob", c.Actor, "a later event naming an actor replaces the earlier one")
}

// The same failure as the actor case, on Title. An unconditional assignment does not
// merely blank a recorded title — the empty-title fallback then substitutes the LATER
// event's raw type, so "Deploy v2.5.0 to prod" becomes "deployment.annotated". That is
// worse than an empty field: it is a plausible-looking wrong answer, and it reaches the
// egress JSON, the Grafana annotation title and the OTLP span name.
//
// Title is optional on both ingest paths, so a titleless follow-up event is the common
// case rather than an edge case.
func TestFoldPreservesTitleAgainstAnEventCarryingNone(t *testing.T) {
	titled := ev("01A", "dep-7", "deployment.started", t10)
	titled.Title = "Deploy v2.5.0 to prod"
	untitled := ev("01B", "dep-7", "deployment.annotated", t20)
	untitled.Title = ""

	c := core.Fold(nil, titled)
	require.Equal(t, "Deploy v2.5.0 to prod", c.Title, "precondition: the first event titles it")
	c = core.Fold(&c, untitled)
	assert.Equal(t, "Deploy v2.5.0 to prod", c.Title,
		"a titleless event must not replace a recorded title with its own type")

	// Like Actor, the unconditional form made Title depend on fold order.
	rev := core.Fold(nil, untitled)
	rev = core.Fold(&rev, titled)
	assert.Equal(t, c.Title, rev.Title, "Title must not depend on fold order")
}

// The type fallback still applies when no event has ever carried a title, so a change
// always renders with something rather than an empty string.
func TestFoldTitleStillFallsBackToTypeWhenNoEventTitlesIt(t *testing.T) {
	untitled := ev("01A", "dep-7", "deployment.started", t10)
	untitled.Title = ""

	c := core.Fold(nil, untitled)
	assert.Equal(t, "deployment.started", c.Title,
		"with no title recorded, the event's own type is the fallback")
}

// A later event that DOES carry a title replaces the earlier one.
func TestFoldLaterNonEmptyTitleWins(t *testing.T) {
	first := ev("01A", "dep-7", "deployment.started", t10)
	first.Title = "Deploy v2.5.0"
	second := ev("01B", "dep-7", "deployment.completed", t20)
	second.Title = "Deploy v2.5.1"

	c := core.Fold(nil, first)
	c = core.Fold(&c, second)
	assert.Equal(t, "Deploy v2.5.1", c.Title)
}

// TestFoldUnionsSourcesWhateverTheOrder pins the rule that makes a source set safe to
// project: it is a union, so folding a ref's events in either order gives the same answer.
// Last-wins did not have this property, which is why source used to appear in rebuild.go's
// list of fields a rebuild can move.
func TestFoldUnionsSourcesWhateverTheOrder(t *testing.T) {
	at := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	ci := core.Event{
		ID: "01K5A000000000000000000001", Tenant: "t", Type: "deployment.started",
		Source: "github", Subject: "acme/prod", OccurredAt: at,
		CorrelationRef: core.RefFromRow("ci/dep-1"),
	}
	flux := core.Event{
		ID: "01K5A000000000000000000002", Tenant: "t", Type: "deployment.completed",
		Source: "flux", Subject: "acme/prod", OccurredAt: at.Add(time.Minute),
		CorrelationRef: core.RefFromRow("ci/dep-1"),
	}

	forward := core.Fold(nil, ci)
	forward = core.Fold(&forward, flux)
	backward := core.Fold(nil, flux)
	backward = core.Fold(&backward, ci)

	assert.Equal(t, []string{"flux", "github"}, forward.Sources,
		"both systems that reported the change are kept, sorted")
	assert.Equal(t, forward.Sources, backward.Sources,
		"a union is commutative, so replay order cannot change the answer")
}

// TestFoldDoesNotRepeatASource keeps the set a set: the common case is many events from one
// system, and an array that grew per event would be unbounded.
func TestFoldDoesNotRepeatASource(t *testing.T) {
	at := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	ev := func(id string) core.Event {
		return core.Event{
			ID: id, Tenant: "t", Type: "deployment.started", Source: "github",
			Subject: "acme/prod", OccurredAt: at, CorrelationRef: core.RefFromRow("ci/dep-1"),
		}
	}
	c := core.Fold(nil, ev("01K5A000000000000000000001"))
	c = core.Fold(&c, ev("01K5A000000000000000000002"))
	assert.Equal(t, []string{"github"}, c.Sources)
}

func foldPtr(c core.Change) *core.Change { return &c }

func TestFoldKeepsEachSubjectsEarliestTime(t *testing.T) {
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	mk := func(subject string, d time.Duration) core.Event {
		return core.Event{Tenant: "t", Locus: core.LocusFromRow("flux"),
			ID: core.NewULID(at, rand.Reader), Type: "x.annotated", Subject: subject,
			OccurredAt: at.Add(d), CorrelationRef: core.RefFromRow("flux/r"),
			Lifecycle: core.LifecycleNone}
	}
	a, b, aEarly := mk("Deployment/a/x", time.Minute), mk("Service/a/y", 2*time.Minute),
		mk("Deployment/a/x", 0)

	forward := core.Fold(foldPtr(core.Fold(foldPtr(core.Fold(nil, a)), b)), aEarly)
	backward := core.Fold(foldPtr(core.Fold(foldPtr(core.Fold(nil, aEarly)), b)), a)

	want := map[string]int64{
		"Deployment/a/x": at.UnixMicro(),
		"Service/a/y":    at.Add(2 * time.Minute).UnixMicro(),
	}
	assert.Equal(t, want, forward.SubjectFirstSeen)
	assert.Equal(t, want, backward.SubjectFirstSeen, "a running minimum is order-independent")
}

func TestFoldNeverSharesTheSubjectMapWithCur(t *testing.T) {
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	cur := core.Change{SubjectFirstSeen: map[string]int64{"s": at.UnixMicro()}}
	core.Fold(&cur, core.Event{Tenant: "t", Locus: core.LocusFromRow("flux"),
		ID: core.NewULID(at, rand.Reader), Type: "x.annotated", Subject: "other",
		OccurredAt: at, Lifecycle: core.LifecycleNone, CorrelationRef: core.RefFromRow("flux/r")})
	assert.Equal(t, map[string]int64{"s": at.UnixMicro()}, cur.SubjectFirstSeen)
}

func TestFoldStartsWithAnEmptyNotNilSubjectMap(t *testing.T) {
	now := time.Now()
	got := core.Fold(nil, core.Event{Tenant: "t", Locus: core.LocusFromRow("ci"),
		Type: "x.started", OccurredAt: now, ID: core.NewULID(now, rand.Reader)})
	assert.NotNil(t, got.SubjectFirstSeen, "pgx writes a nil map as NULL, which NOT NULL refuses")
}
