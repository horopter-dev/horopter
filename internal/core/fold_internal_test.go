package core

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFoldLifecycleTransitions asserts every cell of transitions, walking the table for coverage.
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
	// Reached through the ordinary fold: a hand-built state can be one no fold produces.
	opened := func() Change { return Fold(nil, corr("01A", "deployment.started", "", at)) }
	closed := func() Change {
		c := opened()
		return Fold(&c, corr("01B", "deployment.completed", "", at.Add(time.Minute)))
	}

	// Expectations are written by hand: reading them from the cell would be a tautology.
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
		// wantStart is true: the prior fold set StartedAt and this cell leaves it alone.
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

	// Coverage, and only coverage, is read from the table.
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

// TestFoldStatusForLifecycleIsTotalOverLifecycles keeps every Lifecycle folding to a Status.
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

// TestFoldLifecycleCommutativeCellsAgreeInBothOrders folds each commutative cell both ways.
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

	// open/none-standalone recurs only as the same event replayed, so it claims idempotence.
	seen[[2]int{int(stateOpen), int(assertsNothingStandalone)}] = true
	t.Run("open/none-standalone", func(t *testing.T) {
		require.True(t, transitions[stateOpen][assertsNothingStandalone].commutative,
			"this case claims a cell that does not claim commutativity")

		x := Event{ID: "01H", Tenant: "t", Type: "config.changed", Source: "ci",
			Subject: "s", OccurredAt: at, Locus: LocusFromRow("ci")}
		once := Fold(nil, x)
		twice := Fold(&once, x)

		// The replay lands in the unreachable closed cell, which keeps twice equal to once.
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

// TestClassifyLetsAnAssertionOverrideTheTypesSuffix: an explicit start beats a closing suffix.
func TestClassifyLetsAnAssertionOverrideTheTypesSuffix(t *testing.T) {
	e := Event{ID: "01A", Tenant: "t", Type: "deployment.completed", Source: "ci",
		Subject: "s", OccurredAt: time.Now(), Locus: LocusFromRow("ci"),
		Lifecycle: LifecycleStarted}
	a, asserted := classify(e)
	assert.Equal(t, assertsStart, a, "the explicit assertion wins over the type suffix")
	assert.Empty(t, asserted, "a start asserts no terminal status")
}

// TestClassifyTreatsLifecycleAndIdentityAsOrthogonal: a correlated none is in flight.
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
