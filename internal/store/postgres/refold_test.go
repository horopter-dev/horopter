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

// TestAnOutOfOrderAppendConvergesTheRefToIDOrder is the whole point of this work, and it
// uses the one fixture where the two fold orders give different ANSWERS rather than
// different bookkeeping.
//
// TWO TERMINAL EVENTS. core.Fold's terminal cell is deliberately non-commutative — see
// stateClosed/assertsTerminal in internal/core/fold.go's transitions table — so whichever
// terminal event a ref folds LAST overwrites the status; nothing guards a second terminal
// event from re-closing an already-closed change. completed carries the LOWER id and
// failed the HIGHER, but failed arrives FIRST:
//
//   - incremental: failed closes the change; completed then folds last and overwrites it.
//     Answer: completed.
//   - id-ordered: completed folds first (lower id); failed then folds last and overwrites
//     it. Answer: failed.
//
// This is exactly the flip RebuildProjection's doc comment warns an operator about, and
// asserting on it is what makes this test fail when the re-fold is removed. A
// started/completed fixture would NOT: a late start cannot reopen a closed change under
// either order, so its status assertion passes whether the re-fold runs or not.
func TestAnOutOfOrderAppendConvergesTheRefToIDOrder(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	completed := mkEvent(tn, eventID(t, "completed"), "k2", "d1", "deployment.completed",
		at.Add(20*time.Minute))
	failed := mkEvent(tn, eventID(t, "failed"), "k1", "d1", "deployment.failed", at)
	require.Less(t, completed.ID, failed.ID, "fixture: ids must sort in build order")

	// Reverse arrival: the HIGHER-id failure commits first.
	_, _, err := s.AppendEvent(ctx, failed)
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, completed)
	require.NoError(t, err)

	got := oneChange(ctx, t, s, tn, "ci/d1")
	assert.Equal(t, core.StatusFailed, got.Status,
		"id order closes on failed; without the re-fold this is completed")
	assert.Equal(t, failed.ID, got.LastEventID,
		"last_event_id must be the highest id folded, not the last one to arrive")
	assert.True(t, got.Start.Equal(at), "start is a minimum and must be the earlier time")
}

// TestAConvergedRefEqualsAWholeTenantRebuild is the invariant stated as an equality
// rather than as a list of fields. Whatever the two orders disagree about, they must not
// disagree after this change: a rebuild replays the same log in the same id order, so it
// must find nothing to alter.
func TestAConvergedRefEqualsAWholeTenantRebuild(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	built := make([]core.Event, 0, 6)
	for i, typ := range []string{
		"deployment.started", "deployment.annotated", "deployment.annotated",
		"deployment.failed", "deployment.annotated", "deployment.completed",
	} {
		ev := mkEvent(tn, eventID(t, typ), uniqueToken(), "d1", typ,
			at.Add(time.Duration(i)*time.Minute))
		ev.Attributes = map[string]any{"step": i, "shared": typ}
		built = append(built, ev)
	}

	// Append in an order that disagrees with id order at several points.
	for _, i := range []int{3, 0, 5, 1, 4, 2} {
		_, _, err := s.AppendEvent(ctx, built[i])
		require.NoError(t, err)
	}

	live := oneChange(ctx, t, s, tn, "ci/d1")
	require.NoError(t, s.RebuildProjection(ctx, tn))
	rebuilt := oneChange(ctx, t, s, tn, "ci/d1")

	assert.Equal(t, live, rebuilt,
		"a rebuild found something to change, so live folding is still not id-ordered")
}

// TestARetriedOutOfOrderAppendDoesNotRefoldAgain pins that the dedupe path still returns
// before the projection is touched. A producer retrying a late event must not pay for a
// second re-fold, and must not be able to move the projection by retrying.
//
// started must carry the LOWER id and completed the higher one, with completed appended
// first: that is what makes started's own (first, non-retry) append the out-of-order one.
// The fold.out_of_order assertion below checks that directly rather than assuming it —
// with the ids the other way around, started's append is in order, refoldRef is never
// called even once, and the test would pass without exercising the dedupe/re-fold
// interaction its name promises.
func TestARetriedOutOfOrderAppendDoesNotRefoldAgain(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()

	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	started := mkEvent(tn, eventID(t, "started"), "k2", "d1", "deployment.started", at)
	completed := mkEvent(tn, eventID(t, "completed"), "k1", "d1", "deployment.completed",
		at.Add(20*time.Minute))
	require.Less(t, started.ID, completed.ID, "fixture: ids must sort in build order")

	_, _, err := s.AppendEvent(ctx, completed)
	require.NoError(t, err)
	_, created, err := s.AppendEvent(ctx, started)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, []string{"false", "true"},
		spanAttrValues(rec, "AppendEvent", "horopter.fold.out_of_order"),
		"fixture: started's own append must be the one that takes the re-fold arm")

	before := oneChange(ctx, t, s, tn, "ci/d1")
	_, created, err = s.AppendEvent(ctx, started)
	require.NoError(t, err)
	assert.False(t, created, "the retry must be deduplicated")
	assert.Equal(t, before, oneChange(ctx, t, s, tn, "ci/d1"),
		"a deduplicated retry must not have re-folded the ref")
}

// TestAnOutOfOrderAppendStillFailsFastUnderARebuildLock pins that adding a re-fold inside
// the append transaction did not change how an append composes with a rebuild. The
// re-fold runs under locks the append already holds, so it must add no new wait — an
// append blocked by a rebuild must still give up on AppendLockTimeout with a retryable
// error rather than deadlocking against its own re-fold.
func TestAnOutOfOrderAppendStillFailsFastUnderARebuildLock(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	// started must carry the LOWER id and completed the higher one, with completed
	// appended first — the same fixture shape TestARetriedOutOfOrderAppendDoesNotRefoldAgain
	// uses — so started's blocked append below is genuinely the out-of-order one the test
	// name and the comment describe, not one that happens to take the in-order arm anyway.
	started := mkEvent(tn, eventID(t, "started"), "k2", "d1", "deployment.started", at)
	completed := mkEvent(tn, eventID(t, "completed"), "k1", "d1", "deployment.completed",
		at.Add(20*time.Minute))
	require.Less(t, started.ID, completed.ID, "fixture: ids must sort in build order")
	_, _, err := s.AppendEvent(ctx, completed)
	require.NoError(t, err)

	// Hold the tenant lock exclusively, as a rebuild does, in a transaction of our own.
	tx, err := s.Pool().Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, tn)
	require.NoError(t, err)

	// This append is out of order, so it would re-fold — but it cannot get the lock.
	start := time.Now()
	_, _, err = s.AppendEvent(ctx, started)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 3*postgres.AppendLockTimeout,
		"the append must give up on its own timeout, not wait on the re-fold")
	assert.Contains(t, err.Error(), "retry the append")
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
//
// The real catcher is the require.NoError on RefoldRef, not the assert.Empty below it: if
// refoldRef dropped its events == 0 branch and called upsertChange unconditionally on a
// zero-value foldedRef.change, the write would fail the tenant foreign key, and that
// failure would surface at the require.NoError. Do not simplify that call away.
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

// TestRefoldRefReadsOnlyItsOwnRef pins ref scoping by correctness, not by cost: events
// belonging to other refs of the same tenant must not reach this change, even while a
// far larger log for the same tenant is present. It does NOT pin the spec's cost claim
// that a re-fold reads only one ref's events — if foldRef's predicate regressed to
// `WHERE tenant = $1`, scanning the whole tenant log, foldRows would still bucket the
// noise events under their own refs (ci/other-0 .. ci/other-5) and acc["ci/d1"] would be
// byte-identical, so this test would pass unchanged. What it does catch is a predicate
// that matches the wrong rows or none: that drives folded.events to 0, takes the delete
// branch, and fails oneChange's require.Len. The cost bound is left unasserted on
// purpose — a timing assertion in a suite that shares one database is a flake waiting to
// happen.
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
