package core

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFoldLifecycleTransitions walks every cell of transitions and asserts what folding
// through it produces. Driving off the real table rather than a copy is the point: a cell
// added to transitions with no case here fails the exhaustiveness check at the bottom,
// where a hand-written list would simply not mention it.
func TestFoldLifecycleTransitions(t *testing.T) {
	at := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	later := at.Add(time.Hour)

	// corr builds a correlated event; alone builds a standalone one.
	corr := func(id, typ string, lc Lifecycle, when time.Time) Event {
		return Event{ID: id, Tenant: "t", Type: typ, Source: "ci", Subject: "s",
			OccurredAt: when, Locus: LocusFromRow("ci"), Lifecycle: lc,
			CorrelationRef: RefFromRow("ci/dep-1")}
	}
	alone := func(id, typ string, lc Lifecycle, when time.Time) Event {
		return Event{ID: id, Tenant: "t", Type: typ, Source: "ci", Subject: "s",
			OccurredAt: when, Locus: LocusFromRow("ci"), Lifecycle: lc}
	}
	// opened and closed reach the two states through the ordinary fold, not by
	// constructing a Change by hand — a hand-built state can be one no fold produces.
	opened := func() Change { return Fold(nil, corr("01A", "deployment.started", "", at)) }
	closed := func() Change {
		c := opened()
		return Fold(&c, corr("01B", "deployment.completed", "", at.Add(time.Minute)))
	}

	// Expectations are written by HAND, not read from transitions. Reading the cell to
	// decide what to assert would make the test a tautology: mutating a cell would move
	// the expectation and the behaviour together and nothing would fail. The table is
	// walked below for COVERAGE only.
	cases := map[string]struct {
		state      changeState
		assert     assertion
		prior      func() Change // nil for a change that does not exist yet
		ev         Event
		wantStatus Status
		wantEnded  bool
		wantStart  bool
	}{
		"open/start-by-convention": {stateOpen, assertsStart, nil,
			alone("01C", "deployment.started", "", later),
			StatusInProgress, false, true},
		"open/start-explicit": {stateOpen, assertsStart, nil,
			alone("01D", "deployment.initiated", LifecycleStarted, later),
			StatusInProgress, false, true},
		"open/terminal-by-convention": {stateOpen, assertsTerminal, nil,
			alone("01E", "incident.resolved", "", later),
			StatusCompleted, true, false},
		"open/terminal-explicit": {stateOpen, assertsTerminal, nil,
			alone("01F", "deployment.oops", LifecycleFailed, later),
			StatusFailed, true, false},
		"open/none-standalone": {stateOpen, assertsNothingStandalone, nil,
			alone("01G", "config.changed", "", later),
			StatusOccurred, true, false},
		"open/none-standalone-explicit": {stateOpen, assertsNothingStandalone, nil,
			alone("01H", "deployment.started", LifecycleNone, later),
			StatusOccurred, true, false},
		// wantStart is TRUE: the prior fold opened the change with a start assertion, and
		// this cell's minStart is false, so foldLifecycle leaves StartedAt exactly as it
		// found it — set. Every prior that reaches a state carries its own effects in.
		"open/none-correlated": {stateOpen, assertsNothingCorrelated, opened,
			corr("01I", "deployment.annotated", "", later),
			StatusInProgress, false, true},
		"closed/start": {stateClosed, assertsStart, closed,
			corr("01J", "deployment.started", "", at.Add(-time.Hour)),
			StatusCompleted, true, true},
		"closed/terminal": {stateClosed, assertsTerminal, closed,
			corr("01K", "deployment.failed", "", later),
			StatusFailed, true, true},
		"closed/none-correlated": {stateClosed, assertsNothingCorrelated, closed,
			corr("01L", "deployment.annotated", "", later),
			StatusCompleted, true, true},
	}

	seen := map[[2]int]bool{}
	for name, tc := range cases {
		seen[[2]int{int(tc.state), int(tc.assert)}] = true
		t.Run(name, func(t *testing.T) {
			gotAssert, _ := classify(tc.ev)
			require.Equal(t, tc.assert, gotAssert,
				"classify must reach the cell this case names")

			var got Change
			if tc.prior == nil {
				got = Fold(nil, tc.ev)
			} else {
				prior := tc.prior()
				require.Equal(t, tc.state == stateClosed, prior.End != nil,
					"precondition: the prior fold must reach the state under test")
				got = Fold(&prior, tc.ev)
			}

			assert.Equal(t, tc.wantStatus, got.Status)
			assert.Equal(t, tc.wantEnded, got.End != nil, "End set?")
			assert.Equal(t, tc.wantStart, got.StartedAt != nil, "StartedAt set?")
		})
	}

	// Coverage, and ONLY coverage, is read from the table. A cell with a rule and no case
	// is a rule nobody exercised.
	for st := changeState(0); st < numStates; st++ {
		for a := assertion(0); a < numAssertions; a++ {
			if transitions[st][a].status == unreachable {
				continue
			}
			assert.True(t, seen[[2]int{int(st), int(a)}],
				"transitions[%d][%d] has a rule and no test case", st, a)
		}
	}
}

// TestFoldStatusForLifecycleIsTotalOverLifecycles folds a standalone event carrying each
// value in core.Lifecycles and asserts the resulting Change.Status is always one of
// core.Statuses. statusForLifecycle enumerates four terminal values and returns "" for
// anything else, calling that unreachable — but nothing pinned it as total over Lifecycles
// until this test: classify routes every Lifecycle that is not Started, None or Unspecified
// to assertsTerminal with statusForLifecycle(ev.Lifecycle), so an eighth Lifecycle added to
// core.Lifecycles without a matching case here would fold to Status == "", and
// migration 0002's change_status_vocabulary CHECK would reject it on the next append.
func TestFoldStatusForLifecycleIsTotalOverLifecycles(t *testing.T) {
	at := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	for _, lc := range Lifecycles {
		ev := Event{ID: "01M", Tenant: "t", Type: "widget.frobnicated", Source: "ci",
			Subject: "s", OccurredAt: at, Locus: LocusFromRow("ci"), Lifecycle: lc}
		got := Fold(nil, ev)
		assert.True(t, slices.Contains(Statuses, got.Status),
			"Lifecycle %q folded to Status %q, which is not in core.Statuses", lc, got.Status)
	}
}

// TestFoldLifecycleCommutativeCellsAgreeInBothOrders enforces cell.commutative rather than
// leaving it a comment with extra syntax. A cell claiming commutativity that nothing folds
// backwards is a claim nobody checked.
//
// One pair per claiming cell, and the coverage loop at the bottom reads .commutative from
// the table — so a cell that starts claiming it without gaining a pair here fails, and the
// terminal cells are excluded by the field rather than by a list that can drift.
func TestFoldLifecycleCommutativeCellsAgreeInBothOrders(t *testing.T) {
	at := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	later := at.Add(time.Hour)
	corr := func(id, typ string, lc Lifecycle, when time.Time) Event {
		return Event{ID: id, Tenant: "t", Type: typ, Source: "ci", Subject: "s",
			OccurredAt: when, Locus: LocusFromRow("ci"), Lifecycle: lc,
			CorrelationRef: RefFromRow("ci/dep-1")}
	}
	closed := func() Change {
		c := Fold(nil, corr("01A", "deployment.started", "", at))
		return Fold(&c, corr("01B", "deployment.completed", "", at.Add(time.Minute)))
	}

	pairs := map[string]struct {
		state  changeState
		assert assertion
		prior  func() Change
		x, y   Event
	}{
		"open/start": {stateOpen, assertsStart, nil,
			corr("01C", "deployment.started", "", at),
			corr("01D", "deployment.started", "", later)},
		"open/none-correlated": {stateOpen, assertsNothingCorrelated,
			func() Change { return Fold(nil, corr("01E", "deployment.started", "", at)) },
			corr("01F", "deployment.annotated", "", at),
			corr("01G", "deployment.annotated", "", later)},
		"closed/start": {stateClosed, assertsStart, closed,
			corr("01I", "deployment.started", "", at.Add(-time.Hour)),
			corr("01J", "deployment.started", "", at.Add(-2*time.Hour))},
		"closed/none-correlated": {stateClosed, assertsNothingCorrelated, closed,
			corr("01K", "deployment.annotated", "", at),
			corr("01L", "deployment.annotated", "", later)},
	}

	seen := map[[2]int]bool{}
	for name, tc := range pairs {
		seen[[2]int{int(tc.state), int(tc.assert)}] = true
		t.Run(name, func(t *testing.T) {
			require.True(t, transitions[tc.state][tc.assert].commutative,
				"this case claims a cell that does not claim commutativity")

			fold := func(first, second Event) Change {
				var c Change
				if tc.prior == nil {
					c = Fold(nil, first)
				} else {
					p := tc.prior()
					c = Fold(&p, first)
				}
				return Fold(&c, second)
			}
			forward, backward := fold(tc.x, tc.y), fold(tc.y, tc.x)

			assert.Equal(t, forward.Status, backward.Status, "status must not depend on order")
			assert.Equal(t, forward.StartedAt, backward.StartedAt,
				"the earliest start assertion wins in either order")
			assert.Equal(t, forward.End, backward.End)
		})
	}

	// open/none-standalone does not fit the forward/backward shape above: folding a
	// standalone none-assertion sets End (setsEnd is true even in the open cell), so a
	// second, DISTINCT standalone-none event folded after the first never lands in this
	// same cell again — it lands in stateClosed, a different cell entirely. The only
	// event that can ever reach this cell twice is the SAME event id replayed onto its
	// own change, which makes the claim here idempotence, not commutativity: folding x
	// again must reproduce the change folding it once already produced.
	seen[[2]int{int(stateOpen), int(assertsNothingStandalone)}] = true
	t.Run("open/none-standalone", func(t *testing.T) {
		require.True(t, transitions[stateOpen][assertsNothingStandalone].commutative,
			"this case claims a cell that does not claim commutativity")

		x := Event{ID: "01H", Tenant: "t", Type: "config.changed", Source: "ci",
			Subject: "s", OccurredAt: at, Locus: LocusFromRow("ci")}
		once := Fold(nil, x)
		twice := Fold(&once, x)

		// Replaying x is what reaches transitions[stateClosed][assertsNothingStandalone]:
		// once.End is already set, so the second fold sees stateClosed, and that cell is
		// the zero value — unreachable — which is the ONLY thing that keeps a replayed
		// event from re-triggering the open cell's effects a second time. If that cell
		// stopped being unreachable, twice would diverge from once.
		assert.Equal(t, once.Status, twice.Status,
			"folding the same event again must not change the status it already set")
		assert.Equal(t, once.End, twice.End,
			"folding the same event again must not move End")
		assert.Equal(t, once.StartedAt, twice.StartedAt,
			"folding the same event again must not touch StartedAt")
	})

	// Coverage read from the table: every cell CLAIMING commutativity has a pair here.
	for st := changeState(0); st < numStates; st++ {
		for a := assertion(0); a < numAssertions; a++ {
			if !transitions[st][a].commutative {
				continue
			}
			assert.True(t, seen[[2]int{int(st), int(a)}],
				"transitions[%d][%d] claims commutative and nothing folds it backwards", st, a)
		}
	}
}

// TestClassifyLetsAnAssertionOverrideTheTypesSuffix is explicit-beats-convention in the
// direction that matters: the producer's own vocabulary says this event CLOSED the change,
// and the explicit assertion says it opened one. The assertion wins, and the suffix is
// never consulted — which is what makes the field an override rather than a tiebreak.
func TestClassifyLetsAnAssertionOverrideTheTypesSuffix(t *testing.T) {
	e := Event{ID: "01A", Tenant: "t", Type: "deployment.completed", Source: "ci",
		Subject: "s", OccurredAt: time.Now(), Locus: LocusFromRow("ci"),
		Lifecycle: LifecycleStarted}
	a, asserted := classify(e)
	assert.Equal(t, assertsStart, a, "the explicit assertion wins over the type suffix")
	assert.Empty(t, asserted, "a start asserts no terminal status")
}

// TestClassifyTreatsLifecycleAndIdentityAsOrthogonal is the spec's whole thesis in one
// assertion: an assertion means the same thing whether or not a correlation ref came with
// it, and a correlated "none" is in flight rather than a point.
func TestClassifyTreatsLifecycleAndIdentityAsOrthogonal(t *testing.T) {
	base := Event{ID: "01A", Tenant: "t", Type: "config.changed", Source: "ci",
		Subject: "s", OccurredAt: time.Now(), Locus: LocusFromRow("ci"),
		Lifecycle: LifecycleNone}

	a, _ := classify(base)
	assert.Equal(t, assertsNothingStandalone, a)

	base.CorrelationRef = RefFromRow("ci/dep-1")
	a, _ = classify(base)
	assert.Equal(t, assertsNothingCorrelated, a,
		"the same assertion with an identity is in flight, not a point")
}
