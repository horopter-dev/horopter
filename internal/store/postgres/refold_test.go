package postgres_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

	// Reverse arrival: second (the higher id) commits first, so first's own append is the
	// out-of-order one and AppendEvent re-folds this ref before RefoldRef is ever called
	// below. Appending in id order, as this test used to, lets mergedAttributes get "env"
	// right by accident — it always takes the last-ARRIVED event's value regardless of id,
	// so an id-ordered arrival can never tell that rule apart from the correct, per-key,
	// last-ID-wins one. Only an out-of-order arrival can, which is what this fixture
	// exists to pin: the live append path converges attributes, not only the explicit
	// RefoldRef call this test is named for.
	_, _, err := s.AppendEvent(ctx, second)
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, first)
	require.NoError(t, err)

	ref := core.RefFromRow("ci/d1")
	converged := oneChange(ctx, t, s, tn, ref.String())
	assert.Equal(t, "prod", converged.Attributes["env"],
		"the higher-id event's value must win at a key both set, even though it arrived "+
			"first")
	assert.Equal(t, "kept", converged.Attributes["only-on-first"],
		"a key only the lower-id event set must survive the merge")

	// RefoldRef on an already-converged ref must reproduce it exactly, the same invariant
	// TestRefoldRefFoldsACorrelatedRefFromItsLog pins for the ordinary, in-order case.
	require.NoError(t, s.RefoldRef(ctx, tn, ref))
	assert.Equal(t, converged, oneChange(ctx, t, s, tn, ref.String()),
		"re-folding an already-converged ref must not change it")
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

// refoldFuzzSeedEnv overrides the shuffle seed
// TestAConvergedRefEqualsARandomlyOrderedRebuild uses. Export it (a base-10 int64) to
// replay one run's exact orderings; left unset, the seed is derived from the wall clock so
// distinct runs explore distinct permutations. Either way, the seed actually in force is
// logged with t.Logf on every run: a property test whose failure cannot be replayed from
// its own output is not worth having.
const refoldFuzzSeedEnv = "HOROPTER_REFOLD_FUZZ_SEED"

// refoldFuzzIterations bounds TestAConvergedRefEqualsARandomlyOrderedRebuild's cost. Each
// iteration appends len(refoldFuzzRecipe) = 6 events, one append transaction each, plus one
// whole-tenant RebuildProjection. Measured against the real suite (task test --
// ./internal/store/postgres/...): the package took 10.95s before this test existed and
// 11.89s with it at 24 iterations, so 24 iterations cost about 0.94s, or roughly 40ms per
// iteration — seconds, not minutes, added to an ~11s suite.
const refoldFuzzIterations = 24

// refoldFuzzRecipe is TestAConvergedRefEqualsARandomlyOrderedRebuild's event set. It starts
// from TestAConvergedRefEqualsAWholeTenantRebuild's six events and its two terminals
// (failed, completed — core.Fold's stateClosed/assertsTerminal cell in
// internal/core/fold.go is {setAsserted, setsEnd: true, commutative: false}, so whichever
// terminal folds LAST overwrites Status and End), and adds a per-event Title and Actor
// (last-NON-EMPTY-wins) plus a "shared" attribute key set to a distinct value by every
// event (merged last-wins per key).
//
// WHY A DRAW DIVERGES UNDER THE DISABLED-REFOLD MUTATION. core.Fold assigns Type and
// LastEventID UNCONDITIONALLY on every fold (out.Type = ev.Type; out.LastEventID = ev.ID —
// no guard, in this fixture and in its unmodified predecessor alike), so whichever event
// folds LAST always sets both. Ids are assigned in recipe order (buildRefoldFuzzEvents), so
// index 5 ("deployment.completed") always carries the highest id and is always folded last
// under id order; it is also the only event whose Type string is unique in this recipe.
// Live, under the mutation, folds in ARRIVAL order — so live matches rebuilt on Type, and
// this fixture shows no divergence, if and only if the drawn permutation's LAST element is
// index 5: 1 of 6 possible positions. That makes P(diverge) = 5/6 exactly and
// E[failures in refoldFuzzIterations=24] = 20 — precisely the 20/24 the mutation proof
// observed (see the report), not approximately, and not because of Title, Actor or shared.
//
// WHAT TITLE, ACTOR AND SHARED ADD. They do not change WHICH draws diverge — Type and
// LastEventID already force that on the same 5/6 of draws, and did so in the unmodified
// predecessor fixture too. They widen what a divergent draw's failure output SHOWS: Title
// and Actor exercise the guarded-non-empty rule, and the shared key exercises per-key
// last-wins on Attributes, so a failing assert.Equal diff carries more of the fields
// alteredFields (refold.go) tracks, not only Type and LastEventID.
var refoldFuzzRecipe = []struct{ typ, title, actor, shared string }{
	{"deployment.started", "deploy started", "alice", "v1"},
	{"deployment.annotated", "", "", "v2"},
	{"deployment.annotated", "annotated by bob", "bob", "v3"},
	{"deployment.failed", "deploy failed", "carol", "v4"},
	{"deployment.annotated", "", "dave", "v5"},
	{"deployment.completed", "deploy completed", "eve", "v6"},
}

// refoldFuzzFixtureRef is the producer ref every event in refoldFuzzRecipe targets.
const refoldFuzzFixtureRef = "d1"

// refoldFuzzSeed resolves the seed TestAConvergedRefEqualsARandomlyOrderedRebuild shuffles
// with: refoldFuzzSeedEnv when it is set and parses, the current time otherwise. fromEnv
// reports which one, so the caller can log a replay hint only when the seed was not
// already supplied by a replay.
func refoldFuzzSeed(t *testing.T) (seed int64, fromEnv bool) {
	t.Helper()
	if v := os.Getenv(refoldFuzzSeedEnv); v != "" {
		seed, err := strconv.ParseInt(v, 10, 64)
		require.NoError(t, err, "%s must be a base-10 int64", refoldFuzzSeedEnv)
		return seed, true
	}
	return time.Now().UnixNano(), false
}

// buildRefoldFuzzEvents builds one fresh copy of refoldFuzzRecipe's events for tenant tn,
// each under its own event id — assigned in recipe order, so ids sort in that order — and
// its own idempotency key. The index is folded into the eventID suffix, not just the type,
// because three recipe entries share the type "deployment.annotated" and an eventID log
// line naming only the type cannot tell those three apart.
func buildRefoldFuzzEvents(t *testing.T, tn string, at time.Time) []core.Event {
	t.Helper()
	built := make([]core.Event, len(refoldFuzzRecipe))
	for i, r := range refoldFuzzRecipe {
		suffix := fmt.Sprintf("%d-%s", i, r.typ)
		ev := mkEvent(tn, eventID(t, suffix), uniqueToken(), refoldFuzzFixtureRef, r.typ,
			at.Add(time.Duration(i)*time.Minute))
		ev.Title = r.title
		ev.Actor = r.actor
		ev.Attributes = map[string]any{"shared": r.shared}
		built[i] = ev
	}
	return built
}

// TestAConvergedRefEqualsARandomlyOrderedRebuild generalises
// TestAConvergedRefEqualsAWholeTenantRebuild into the property the design spec calls the
// entire claim of this change (2026-09-29-per-ref-refold plan, quoting spec §7): "For a set
// of events appended in an arbitrary commit order, the live projection must equal
// foldEventLog's answer over the same log. Generating orderings and asserting that equality
// is the entire claim of this change; every other test is a detail." The test above is one
// sample of that claim — six events in the single hard-coded scramble
// []int{3, 0, 5, 1, 4, 2}. This generates the scramble instead of enumerating one instance
// of it.
//
// TENANT. Each iteration runs in its own t.Run subtest and calls tenant(t) there. tenant(t)
// names the tenant after t.Name(), which already differs per subtest (.…/iter0, .…/iter1,
// …); tenant(t) also appends its own uniqueToken() suffix regardless, so the fresh-tenant
// requirement holds twice over even though only one mechanism is required to hold it once.
//
// WHAT THE EQUALITY PINS. live is written by applyToProjection's in-order arm ->
// upsertChange, or — on the out-of-order arm this whole change adds — by refoldRef ->
// foldRef -> foldRows -> upsertChange. rebuilt is written by RebuildProjection ->
// replayProjection -> foldEventLog -> foldRows -> upsertChange. Both foldRows call sites
// fold events through the SAME core.Fold, so a bug confined to one of Fold's field-level
// rules (say, a broken last-non-empty-wins for Title) moves live and rebuilt identically
// and this assertion cannot see it; internal/core/fold_test.go owns those rules directly.
// What this assertion pins is agreement between the two DRIVERS that decide which code
// path runs at all — the shape of the regression applyToProjection (append.go) introduces
// when its out-of-order arm is skipped. The absolute id-ordered answer for a specific
// fixture is pinned separately, by TestAnOutOfOrderAppendConvergesTheRefToIDOrder. This is
// inherited from TestAConvergedRefEqualsAWholeTenantRebuild above, not new here, but a
// reader is more likely to land on this test, so it is recorded here too.
func TestAConvergedRefEqualsARandomlyOrderedRebuild(t *testing.T) {
	seed, fromEnv := refoldFuzzSeed(t)
	if fromEnv {
		t.Logf("shuffle seed %d (from %s)", seed, refoldFuzzSeedEnv)
	} else {
		t.Logf("shuffle seed %d (export %s=%d to replay this run's exact orderings)",
			seed, refoldFuzzSeedEnv, seed)
	}
	//nolint:gosec // G404: shuffling append order for a property test, not a cryptographic
	// use; the seed is what makes a failure replayable, which crypto/rand would defeat.
	rng := rand.New(rand.NewSource(seed))
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	ref := "ci/" + refoldFuzzFixtureRef

	for iter := range refoldFuzzIterations {
		order := rng.Perm(len(refoldFuzzRecipe))
		t.Run(fmt.Sprintf("iter%d", iter), func(t *testing.T) {
			t.Logf("append order %v", order)
			ctx, s, tn := context.Background(), newStore(t), tenant(t)
			built := buildRefoldFuzzEvents(t, tn, at)

			for _, i := range order {
				_, _, err := s.AppendEvent(ctx, built[i])
				require.NoError(t, err)
			}

			live := oneChange(ctx, t, s, tn, ref)
			require.NoError(t, s.RebuildProjection(ctx, tn))
			rebuilt := oneChange(ctx, t, s, tn, ref)

			assert.Equal(t, live, rebuilt,
				"order %v: a rebuild found something to change, so live folding is still "+
					"not id-ordered", order)
		})
	}
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

	// The equality above passes even if the retry DOES call applyToProjection: a re-fold
	// over an unchanged event set is idempotent, so the row comes back byte-identical
	// either way. horopter.fold.out_of_order is set only inside applyToProjection and
	// stays at its zero value, false, on any path that never reaches it — so a third
	// span reading "false" is what actually pins that the dedupe path returned before
	// applyToProjection ran at all, not merely that its result looked unchanged.
	require.Equal(t, []string{"false", "true", "false"},
		spanAttrValues(rec, "AppendEvent", "horopter.fold.out_of_order"),
		"the deduplicated retry must return before applyToProjection runs at all")
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

// TestARefoldThatMovesAnOutcomeIsCountedAsAltered pins the half of the label that says a
// recorded fact moved. Same two-terminal-event fixture as
// TestAnOutOfOrderAppendConvergesTheRefToIDOrder: completed takes the lower id and failed
// the higher one, with failed appended first, so arrival order yields completed and id
// order yields failed — the re-fold moves the recorded status.
func TestARefoldThatMovesAnOutcomeIsCountedAsAltered(t *testing.T) {
	reader := recordMetrics(t)
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	completed := mkEvent(tn, eventID(t, "completed"), "k2", "d1", "deployment.completed",
		at.Add(20*time.Minute))
	failed := mkEvent(tn, eventID(t, "failed"), "k1", "d1", "deployment.failed", at)
	require.Less(t, completed.ID, failed.ID, "fixture: ids must sort in build order")

	_, _, err := s.AppendEvent(ctx, failed)
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, completed)
	require.NoError(t, err)

	assert.Equal(t, int64(1), refoldCounts(t, reader)["true"],
		"a re-fold that changed the recorded status must be counted as altering")
}

// TestARefoldThatMovesOnlyBookkeepingIsNotCountedAsAltered is the test that makes the
// label worth reading. Two annotations identical in every field core.Fold reads, appended
// out of order: the re-fold moves last_event_id — it always does, since core.Fold assigns
// it unconditionally and the arriving event is by definition the lower id — and nothing
// else. Counting that as "altered" would make the label true for every re-fold and the
// README's claim about it false.
func TestARefoldThatMovesOnlyBookkeepingIsNotCountedAsAltered(t *testing.T) {
	reader := recordMetrics(t)
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	early := mkEvent(tn, eventID(t, "early"), "k1", "d1", "deployment.annotated", at)
	late := mkEvent(tn, eventID(t, "late"), "k2", "d1", "deployment.annotated",
		at.Add(time.Minute))
	require.Less(t, early.ID, late.ID, "fixture: ids must sort in build order")

	_, _, err := s.AppendEvent(ctx, late)
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, early)
	require.NoError(t, err)

	counts := refoldCounts(t, reader)
	assert.Equal(t, int64(1), counts["false"],
		"a re-fold that moved only last_event_id must not be counted as altering")
	assert.Zero(t, counts["true"])
}

// TestARefoldWithUnchangedIntAttributesDoesNotReportAttributesMoved pins sameAttributes's
// fix directly. It does not pin it through the counter: after movedSomethingRecorded's
// narrowing to Status and End, a false "attributes" move here still reports
// altered="false", the same as a true one, so horopter.refold — via refoldCounts — cannot
// tell this fixture's fix from its absence. Only horopter.refold.moved, read from the
// append span, is specific enough to catch it;
// TestARefoldThatMovesOnlyBookkeepingIsNotCountedAsAltered already pins the counter half
// of this scenario, so it is not repeated here.
//
// The drift this pins is NOT a change-row read-back — refoldOutOfOrderAppend no longer has
// one. It is foldRows: refoldRef's re-folded Attributes come from scanning the event
// table's jsonb column, where a JSON number always decodes as a Go float64, while
// applyToProjection's incremental fold takes the out-of-order event's OWN attribute values
// straight from the request, where a JSON number stays whatever Go type the caller sent —
// an int here. Without sameAttributes's JSON-encoding comparison, that type mismatch alone
// makes "attributes" look moved on every out-of-order append whose contested key holds a
// JSON number, whether or not the recorded value actually changed. See sameAttributes's
// own doc comment in refold.go for the mechanism in full.
//
// Both events set the SAME key to the SAME int, so the two fold orders agree on Attributes
// exactly; the only field a re-fold may legitimately move here is last_event_id, which
// moves on every out-of-order re-fold by construction. Asserting the exact expected slice,
// rather than only NotContains("attributes"), also guards against horopter.refold.moved
// going silent: moved stays nil if the attribute is never emitted, and nil fails this
// equality where it would have passed a bare NotContains.
func TestARefoldWithUnchangedIntAttributesDoesNotReportAttributesMoved(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	early := mkEvent(tn, eventID(t, "early"), "k1", "d1", "deployment.annotated", at)
	early.Attributes = map[string]any{"step": 1}
	late := mkEvent(tn, eventID(t, "late"), "k2", "d1", "deployment.annotated",
		at.Add(time.Minute))
	late.Attributes = map[string]any{"step": 1}
	require.Less(t, early.ID, late.ID, "fixture: ids must sort in build order")

	_, _, err := s.AppendEvent(ctx, late)
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, early)
	require.NoError(t, err)

	var moved []string
	for _, span := range rec.Ended() {
		if span.Name() != "AppendEvent" {
			continue
		}
		for _, kv := range span.Attributes() {
			if string(kv.Key) == "horopter.refold.moved" {
				moved = kv.Value.AsStringSlice()
			}
		}
	}
	assert.Equal(t, []string{"last_event_id"}, moved,
		"last_event_id moves on every out-of-order re-fold by construction; attributes "+
			"must not, since both fold orders agree on it — the JSONB int/float64 mismatch "+
			"sameAttributes exists to prevent")
}

// TestARefoldThatMovesOnlyTheEndTimeIsCountedAsAltered catches a regression in
// alteredFields that compares End by nil-ness instead of by VALUE.
//
// TWO TERMINAL EVENTS OF THE SAME TYPE. Status, Subject, Type, Title, Actor and
// Attributes are identical on both sides, so the only recorded fact that moves is WHEN
// the change finished.
//
// core.Fold's terminal cell OVERWRITES End every time it fires — see
// stateClosed/assertsTerminal in internal/core/fold.go's transitions table — so whichever
// terminal event folds LAST sets End, regardless of what closed the change first. Id order
// folds early first: the change is open, so early's terminal cell closes it and sets
// End = early's timestamp. Id order then folds late second: the change is now closed, and
// late is ALSO terminal, so its cell fires again and overwrites End with late's timestamp.
// The re-folded (id-ordered) End is therefore late's timestamp — the LATER one — not
// early's, which is what an incremental, arrival-order-only fold would have left behind
// had late arrived first and early folded onto it last instead.
//
// Comparing End by presence rather than by value reports nothing moved, and a change to
// when the log says work finished becomes invisible to both the counter and the warn log.
func TestARefoldThatMovesOnlyTheEndTimeIsCountedAsAltered(t *testing.T) {
	reader := recordMetrics(t)
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	early := mkEvent(tn, eventID(t, "early"), "k1", "d1", "deployment.completed", at)
	late := mkEvent(tn, eventID(t, "late"), "k2", "d1", "deployment.completed",
		at.Add(20*time.Minute))
	require.Less(t, early.ID, late.ID, "fixture: ids must sort in build order")

	_, _, err := s.AppendEvent(ctx, late)
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, early)
	require.NoError(t, err)

	got := oneChange(ctx, t, s, tn, "ci/d1")
	require.NotNil(t, got.End)
	assert.True(t, got.End.Equal(at.Add(20*time.Minute)),
		"id order folds early first and late second; late is also terminal and "+
			"re-closes the change, overwriting End with its own timestamp")
	assert.Equal(t, int64(1), refoldCounts(t, reader)["true"],
		"the end time moved, which is a recorded fact and must be counted as altering")
}

// TestARefoldThatOnlyLosesSubMicrosecondEndPrecisionIsNotCountedAsAltered pins
// sameInstant's precision fix. Only ONE terminal event exists on this ref — unlike
// TestARefoldThatMovesOnlyTheEndTimeIsCountedAsAltered above, there is no second terminal
// event whose timestamp could legitimately win the overwrite instead. folded.End and
// refolded.End are therefore both derived from completed's own OccurredAt; the only thing
// that can make them differ is precision loss between the two paths that read it.
//
// completed.OccurredAt carries 789 sub-microsecond nanoseconds. folded.End comes from
// core.Fold applied to the event as AppendEvent received it — still holding those 789ns.
// refolded.End comes from refoldRef reading completed back from the event table's
// occurred_at column, a timestamptz that pgx's binary wire encoding has already truncated
// to microseconds on insert, discarding all 789 of them. Comparing the two at Go's full
// nanosecond precision reports "end" moved on a ref whose End was set by exactly one
// event and never disagreed about anything Postgres can actually store.
func TestARefoldThatOnlyLosesSubMicrosecondEndPrecisionIsNotCountedAsAltered(t *testing.T) {
	reader := recordMetrics(t)
	rec, restore := recordSpans(t)
	defer restore()
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	// completed's id must be generated first, so it sorts lower: started is appended
	// first (in order, since the ref is new) and completed second, out of order.
	completed := mkEvent(tn, eventID(t, "completed"), "k2", "d1", "deployment.completed",
		at.Add(20*time.Minute).Add(789*time.Nanosecond))
	started := mkEvent(tn, eventID(t, "started"), "k1", "d1", "deployment.started", at)
	require.Less(t, completed.ID, started.ID,
		"fixture: completed's own append must be the out-of-order one")

	_, _, err := s.AppendEvent(ctx, started)
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, completed)
	require.NoError(t, err)

	var moved []string
	for _, span := range rec.Ended() {
		if span.Name() != "AppendEvent" {
			continue
		}
		for _, kv := range span.Attributes() {
			if string(kv.Key) == "horopter.refold.moved" {
				moved = kv.Value.AsStringSlice()
			}
		}
	}
	assert.Equal(t, []string{"type", "title", "last_event_id"}, moved,
		"completed's End is the same instant on both fold paths down to the microsecond "+
			"Postgres can store; only sub-microsecond noise pgx truncates away on insert "+
			"differs, and that is not a move — type, title and last_event_id move for "+
			"unrelated reasons (started and completed carry different event types) and "+
			"are asserted here only so a silent horopter.refold.moved is not mistaken for "+
			"absence of the end move")

	counts := refoldCounts(t, reader)
	require.Equal(t, int64(1), counts["false"],
		"fixture: the re-fold must have been counted at all before the next assertion "+
			"can mean anything")
	assert.Zero(t, counts["true"],
		"a re-fold that only lost sub-microsecond End precision must not be counted as "+
			"altering")
}

// TestRefoldRefDeletesAChangeWhoseEventsAreGone catches a regression that returns before
// refoldRef's DELETE when folded.events == 0. The delete branch is unreachable through the
// store's own API — nothing deletes an event — so the events are removed directly here.
// Without the branch a per-ref re-fold and a whole-tenant rebuild disagree about such a
// ref, which is the equivalence the branch exists to hold.
func TestRefoldRefDeletesAChangeWhoseEventsAreGone(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	ev := mkEvent(tn, eventID(t, "only"), "k1", "d1", "deployment.started", at)
	_, _, err := s.AppendEvent(ctx, ev)
	require.NoError(t, err)
	require.NotEmpty(t, oneChange(ctx, t, s, tn, "ci/d1"))

	_, err = s.Pool().Exec(ctx, `DELETE FROM event WHERE tenant = $1`, tn)
	require.NoError(t, err)

	require.NoError(t, s.RefoldRef(ctx, tn, core.RefFromRow("ci/d1")))

	q := changeQuery(t, tn, time.Time{}, time.Time{}, 0, nil)
	q.Ref = "ci/d1"
	got, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)
	assert.Empty(t, got, "a ref with no events must leave no change behind")
}

// refoldAltered is the attribute set horopter.refold carries when a re-fold altered the
// change, written out by hand — the same guard appendCounts's constants give
// horopter.event.appends in append_test.go. An assertion against this fails if a key is
// added, which is what makes it the cardinality guard as well: refoldCounts, by design,
// reads only the horopter.refold.altered VALUE and cannot tell "the counter carries
// exactly this boolean" from "the counter carries this boolean plus something else too".
const refoldAltered = "horopter.refold.altered=true"

// TestRefoldCountsOnlyTheAlteredBoolean pins horopter.refold's attribute set by EXACT
// match, the way appendCounts-based tests already pin horopter.event.appends's. The code
// comment on the s.refolds.Add call warns that eight field names would give the counter
// 256 series; this is the test that would actually catch feeding refoldMoved's field-name
// list into that counter instead of only the boolean movedSomethingRecorded computes.
// refoldCounts alone cannot: it reads one attribute key by name and stays silent about
// any other key riding along on the same data point.
func TestRefoldCountsOnlyTheAlteredBoolean(t *testing.T) {
	reader := recordMetrics(t)
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	completed := mkEvent(tn, eventID(t, "completed"), "k2", "d1", "deployment.completed",
		at.Add(20*time.Minute))
	failed := mkEvent(tn, eventID(t, "failed"), "k1", "d1", "deployment.failed", at)
	require.Less(t, completed.ID, failed.ID, "fixture: ids must sort in build order")

	_, _, err := s.AppendEvent(ctx, failed)
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx, completed)
	require.NoError(t, err)

	assert.Equal(t, map[string]int64{refoldAltered: 1}, refoldAttributeSets(t, reader),
		"horopter.refold must carry only horopter.refold.altered; any other key is a "+
			"cardinality regression only an exact-set assertion like this one catches")
}

// refoldAttributeSets returns horopter.refold's count for each attribute set, keyed by
// the set's encoding — the same shape appendCounts returns for horopter.event.appends.
func refoldAttributeSets(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "horopter.refold" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "horopter.refold should be an int64 sum")
			for _, dp := range sum.DataPoints {
				out[dp.Attributes.Encoded(attribute.DefaultEncoder())] += dp.Value
			}
		}
	}
	return out
}

// refoldCounts sums horopter.refold by its horopter.refold.altered label.
func refoldCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "horopter.refold" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "horopter.refold should be an int64 sum")
			for _, dp := range sum.DataPoints {
				v, _ := dp.Attributes.Value("horopter.refold.altered")
				out[v.String()] += dp.Value
			}
		}
	}
	return out
}
