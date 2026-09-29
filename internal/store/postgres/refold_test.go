package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/store/postgres"
)

// TestRefoldRefFoldsACorrelatedRefFromItsLog re-folds a ref whose events were appended
// normally and asserts the answer equals what the live fold wrote. It is the base case
// every other test in this file varies.
func TestRefoldRefFoldsACorrelatedRefFromItsLog(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	for _, e := range []struct {
		suffix, idem, typ string
		off               time.Duration
	}{
		{"started", "k1", "deployment.started", 0},
		{"completed", "k2", "deployment.completed", 20 * time.Minute},
	} {
		ev := mkEvent(tn, eventID(t, e.suffix), e.idem, "d1", e.typ, at.Add(e.off))
		_, _, err := s.AppendEvent(ctx, ev)
		require.NoError(t, err)
	}

	ref := core.RefFromRow("ci/d1")
	before := oneChange(ctx, t, s, tn, ref.String())
	require.NoError(t, s.RefoldRef(ctx, tn, ref))
	assert.Equal(t, before, oneChange(ctx, t, s, tn, ref.String()),
		"re-folding a ref nothing has disturbed must not change it")
}

// TestRefoldRefReproducesAttributeMergeAtAContestedKey pins the one field whose rule is
// last-wins PER KEY. decisions/2026-09-16 rejected making every field's rule commutative
// precisely because attributes cannot be done incrementally without per-key provenance,
// so a re-fold is the only thing that gets this right — and it has to.
func TestRefoldRefReproducesAttributeMergeAtAContestedKey(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	first := mkEvent(tn, eventID(t, "first"), "k1", "d1", "deployment.started", at)
	first.Attributes = map[string]any{"env": "staging", "only-on-first": "kept"}
	second := mkEvent(tn, eventID(t, "second"), "k2", "d1", "deployment.completed",
		at.Add(time.Minute))
	second.Attributes = map[string]any{"env": "prod"}
	// "prod" wins only if second holds the higher id. eventID guarantees that; this says
	// the test depends on it, so a change to eventID fails here and not in the fold.
	require.Less(t, first.ID, second.ID, "fixture: ids must sort in build order")

	_, _, err := s.AppendEvent(ctx, first)
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, second)
	require.NoError(t, err)

	ref := core.RefFromRow("ci/d1")
	require.NoError(t, s.RefoldRef(ctx, tn, ref))

	got := oneChange(ctx, t, s, tn, ref.String())
	assert.Equal(t, "prod", got.Attributes["env"],
		"the higher-id event's value must win at a key both set")
	assert.Equal(t, "kept", got.Attributes["only-on-first"],
		"a key only the lower-id event set must survive the merge")
}

// TestRefoldRefIgnoresAPointEventInAnotherLocus is the membership guard. foldRef's query
// matches a point event on its id alone, so a point whose id equals a correlated ref's
// producer half IS read by that query. Keying by core.RefFor is what keeps it out: the
// point keys under its own locus, which is a different ref.
func TestRefoldRefIgnoresAPointEventInAnotherLocus(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	// A point event in locus "ops". Its ref is ops/<id>; its id is <id>.
	point := mkEvent(tn, eventID(t, "point"), "k1", "", "config.changed", at)
	point.Locus = core.LocusFromRow("ops")
	_, _, err := s.AppendEvent(ctx, point)
	require.NoError(t, err)

	// A correlated ref in locus "ci" whose PRODUCER half is that same id, so
	// foldRef("ci/<id>") reads the point event through the `correlation_ref = ''
	// AND id = $3` arm.
	correlated := mkEvent(tn, eventID(t, "correlated"), "k2", point.ID,
		"deployment.started", at.Add(time.Minute))
	_, _, err = s.AppendEvent(ctx, correlated)
	require.NoError(t, err)

	ref := core.RefFromRow("ci/" + point.ID)
	require.NoError(t, s.RefoldRef(ctx, tn, ref))

	got := oneChange(ctx, t, s, tn, ref.String())
	assert.Equal(t, "deployment.started", got.Type,
		"the point event in locus ops must not have folded into the ci ref")
	assert.Equal(t, core.StatusInProgress, got.Status,
		"folding the point in would have closed the change as an occurred point")
}

// oneChange reads exactly one change by ref, failing the test if the projection holds
// any other number.
func oneChange(ctx context.Context, t *testing.T, s *postgres.Store, tn, ref string) core.Change {
	t.Helper()
	q := changeQuery(t, tn, time.Time{}, time.Time{}, 0, nil)
	q.Ref = ref
	got, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)
	require.Len(t, got, 1, "expected exactly one change at ref %q", ref)
	return got[0]
}

// TestRefoldRefFoldsAPointRefFromItsOwnEvent re-folds a POINT ref — one whose events
// carry no correlation ref, so core.RefFor composes the ref from the event's locus and
// its own id. It is the only test that exercises foldRef's second arm as the arm that
// finds the change, rather than as the arm that must not.
//
// Nothing else covers it. TestRefoldRefIgnoresAPointEventInAnotherLocus re-folds a
// CORRELATED ref and asserts a point stayed out, which passes just as well when the point
// arm is broken and the point is never fetched at all.
func TestRefoldRefFoldsAPointRefFromItsOwnEvent(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	ev := mkEvent(tn, eventID(t, "point"), "k1", "", "config.changed", at)
	_, _, err := s.AppendEvent(ctx, ev)
	require.NoError(t, err)

	ref := core.RefFor(ev)
	require.Equal(t, "ci/"+ev.ID, ref.String(), "fixture: a point's ref is locus/own-id")

	before := oneChange(ctx, t, s, tn, ref.String())
	require.NoError(t, s.RefoldRef(ctx, tn, ref))
	after := oneChange(ctx, t, s, tn, ref.String())

	assert.Equal(t, before, after,
		"re-folding a point ref must reproduce it; a broken point arm folds it to "+
			"nothing and deletes the change")
	assert.Equal(t, core.StatusOccurred, after.Status)
}

// TestRefoldRefOnAnUnknownRefWritesNothing pins that a ref with no events is a no-op
// rather than an error or an empty row. Under the decided mechanisms an events count of
// zero means exactly one thing — the ref has never existed — so there is nothing for the
// store to decide and nothing for it to write.
func TestRefoldRefOnAnUnknownRefWritesNothing(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)

	ref := core.RefFromRow("ci/never-existed")
	require.NoError(t, s.RefoldRef(ctx, tn, ref))

	q := changeQuery(t, tn, time.Time{}, time.Time{}, 0, nil)
	q.Ref = ref.String()
	got, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)
	assert.Empty(t, got, "an unknown ref must leave the projection alone")
}

// TestRefoldRefReadsOnlyItsOwnRef pins the cost claim the spec makes: a re-fold is
// bounded by one ref's events, not by the tenant's log. It is asserted through behaviour
// rather than through timing — the ref's change must be correct while a far larger log
// for the same tenant is present, and a whole-log fold would fold those in too.
func TestRefoldRefReadsOnlyItsOwnRef(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	target := mkEvent(tn, eventID(t, "target"), "k-target", "d1", "deployment.started", at)
	_, _, err := s.AppendEvent(ctx, target)
	require.NoError(t, err)

	// Spread across several other refs, so a fold that ignored the ref predicate would
	// have plenty to fold in and would visibly change the answer below.
	for i := range 60 {
		ev := mkEvent(tn, eventID(t, "noise"), uniqueToken(),
			fmt.Sprintf("other-%d", i%6), "deployment.completed",
			at.Add(time.Duration(i)*time.Second))
		_, _, err := s.AppendEvent(ctx, ev)
		require.NoError(t, err)
	}

	ref := core.RefFromRow("ci/d1")
	require.NoError(t, s.RefoldRef(ctx, tn, ref))

	got := oneChange(ctx, t, s, tn, ref.String())
	assert.Equal(t, target.ID, got.LastEventID,
		"the re-fold folded events from another ref into this change")
	assert.Equal(t, core.StatusInProgress, got.Status,
		"a terminal event from another ref would have closed this change")
}
