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
	require.NoError(t, refoldErr(s.RefoldRef(ctx, tn, ref)))
	assert.Equal(t, before, oneChange(ctx, t, s, tn, ref.String()),
		"re-folding a ref nothing has disturbed must not change it")
}

// Attributes are last-wins per key: only a re-fold gets them right.
func TestRefoldRefReproducesAttributeMergeAtAContestedKey(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	first := mkEvent(tn, eventID(t, "first"), "k1", "d1", "deployment.started", at)
	first.Attributes = map[string]any{"env": "staging", "only-on-first": "kept"}
	second := mkEvent(tn, eventID(t, "second"), "k2", "d1", "deployment.completed",
		at.Add(time.Minute))
	second.Attributes = map[string]any{"env": "prod"}
	// Depends on eventID ordering ids by call, so a change to eventID fails here.
	require.Less(t, first.ID, second.ID, "fixture: ids must sort in build order")

	// Reverse arrival: an id-ordered append would get "env" right by accident.
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

	require.NoError(t, refoldErr(s.RefoldRef(ctx, tn, ref)))
	assert.Equal(t, converged, oneChange(ctx, t, s, tn, ref.String()),
		"re-folding an already-converged ref must not change it")
}

// foldRef matches a point on its id alone; keying by core.RefFor keeps it out.
func TestRefoldRefIgnoresAPointEventInAnotherLocus(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	// A point event in locus "ops". Its ref is ops/<id>; its id is <id>.
	point := mkEvent(tn, eventID(t, "point"), "k1", "", "config.changed", at)
	point.Locus = core.LocusFromRow("ops")
	_, _, err := s.AppendEvent(ctx, point)
	require.NoError(t, err)

	// Its producer half equals the point's id, so foldRef reads the point through the id arm.
	correlated := mkEvent(tn, eventID(t, "correlated"), "k2", point.ID,
		"deployment.started", at.Add(time.Minute))
	_, _, err = s.AppendEvent(ctx, correlated)
	require.NoError(t, err)

	ref := core.RefFromRow("ci/" + point.ID)
	require.NoError(t, refoldErr(s.RefoldRef(ctx, tn, ref)))

	got := oneChange(ctx, t, s, tn, ref.String())
	assert.Equal(t, "deployment.started", got.Type,
		"the point event in locus ops must not have folded into the ci ref")
	assert.Equal(t, core.StatusInProgress, got.Status,
		"folding the point in would have closed the change as an occurred point")
}

// Two terminals: the last one folded wins, so the two fold orders give different statuses.
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

// Set to a base-10 int64 to replay a run; the seed in force is always logged.
const refoldFuzzSeedEnv = "HOROPTER_REFOLD_FUZZ_SEED"

// About 40ms per iteration, measured.
const refoldFuzzIterations = 24

// Index 5 has the highest id, so a draw diverges unless it folds last: P = 5/6.
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

func refoldFuzzSeed(t *testing.T) (seed int64, fromEnv bool) {
	t.Helper()
	if v := os.Getenv(refoldFuzzSeedEnv); v != "" {
		seed, err := strconv.ParseInt(v, 10, 64)
		require.NoError(t, err, "%s must be a base-10 int64", refoldFuzzSeedEnv)
		return seed, true
	}
	return time.Now().UnixNano(), false
}

// The index is in the id suffix: three entries share a type.
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

// Pins that the two fold drivers agree; core.Fold's field rules are fold_test.go's job.
func TestAConvergedRefEqualsARandomlyOrderedRebuild(t *testing.T) {
	seed, fromEnv := refoldFuzzSeed(t)
	if fromEnv {
		t.Logf("shuffle seed %d (from %s)", seed, refoldFuzzSeedEnv)
	} else {
		t.Logf("shuffle seed %d (export %s=%d to replay this run's exact orderings)",
			seed, refoldFuzzSeedEnv, seed)
	}
	//nolint:gosec // G404: a seeded shuffle, replayable by design; not cryptographic.
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

// started has the lower id but arrives second, so its first append is out of order.
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

	// Only the third span's out_of_order=false proves the retry never reached applyToProjection.
	require.Equal(t, []string{"false", "true", "false"},
		spanAttrValues(rec, "AppendEvent", "horopter.fold.out_of_order"),
		"the deduplicated retry must return before applyToProjection runs at all")
}

func oneChange(ctx context.Context, t *testing.T, s *postgres.Store, tn, ref string) core.Change {
	t.Helper()
	q := changeQuery(t, tn, time.Time{}, time.Time{}, 0, nil)
	q.Ref = ref
	got, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)
	require.Len(t, got, 1, "expected exactly one change at ref %q", ref)
	return got[0]
}

// The only test where foldRef's point arm is the one that finds the change.
func TestRefoldRefFoldsAPointRefFromItsOwnEvent(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	ev := mkEvent(tn, eventID(t, "point"), "k1", "", "config.changed", at)
	_, _, err := s.AppendEvent(ctx, ev)
	require.NoError(t, err)

	ref := core.RefFor(ev)
	require.Equal(t, "ci/"+ev.ID, ref.String(), "fixture: a point's ref is locus/own-id")

	before := oneChange(ctx, t, s, tn, ref.String())
	require.NoError(t, refoldErr(s.RefoldRef(ctx, tn, ref)))
	after := oneChange(ctx, t, s, tn, ref.String())

	assert.Equal(t, before, after,
		"re-folding a point ref must reproduce it; a broken point arm folds it to "+
			"nothing and deletes the change")
	assert.Equal(t, core.StatusOccurred, after.Status)
}

// A zero-value upsert would fail the tenant FK, not return ErrRefNotFound.
func TestRefoldRefOnAnUnknownRefWritesNothing(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)

	ref := core.RefFromRow("ci/never-existed")
	_, err := s.RefoldRef(ctx, tn, ref)
	require.ErrorIs(t, err, postgres.ErrRefNotFound)

	q := changeQuery(t, tn, time.Time{}, time.Time{}, 0, nil)
	q.Ref = ref.String()
	got, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)
	assert.Empty(t, got, "an unknown ref must leave the projection alone")
}

// Pins scoping by correctness only; a timing assertion would flake on a shared database.
func TestRefoldRefReadsOnlyItsOwnRef(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	target := mkEvent(tn, eventID(t, "target"), "k-target", "d1", "deployment.started", at)
	_, _, err := s.AppendEvent(ctx, target)
	require.NoError(t, err)

	// Spread across other refs, so ignoring the ref predicate changes the answer.
	for i := range 60 {
		ev := mkEvent(tn, eventID(t, "noise"), uniqueToken(),
			fmt.Sprintf("other-%d", i%6), "deployment.completed",
			at.Add(time.Duration(i)*time.Second))
		_, _, err := s.AppendEvent(ctx, ev)
		require.NoError(t, err)
	}

	ref := core.RefFromRow("ci/d1")
	require.NoError(t, refoldErr(s.RefoldRef(ctx, tn, ref)))

	got := oneChange(ctx, t, s, tn, ref.String())
	assert.Equal(t, target.ID, got.LastEventID,
		"the re-fold folded events from another ref into this change")
	assert.Equal(t, core.StatusInProgress, got.Status,
		"a terminal event from another ref would have closed this change")
}

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

// Only last_event_id moves; counting that would mark every re-fold altered.
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

// Only horopter.refold.moved catches this: jsonb numbers decode as float64.
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

// Same-type terminals: only End moves, so End must compare by value.
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

// timestamptz keeps microseconds, so sub-microsecond drift must not count as moved.
func TestARefoldThatOnlyLosesSubMicrosecondEndPrecisionIsNotCountedAsAltered(t *testing.T) {
	reader := recordMetrics(t)
	rec, restore := recordSpans(t)
	defer restore()
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	// completed's id is generated first, so it sorts lower but arrives second.
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

// Nothing deletes an event through the store, so the rows are removed directly.
func TestRefoldRefDeletesAChangeWhoseEventsAreGone(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	ev := mkEvent(tn, eventID(t, "only"), "k1", "d1", "deployment.started", at)
	_, _, err := s.AppendEvent(ctx, ev)
	require.NoError(t, err)
	require.NotEmpty(t, oneChange(ctx, t, s, tn, "ci/d1"))

	_, err = s.Pool().Exec(ctx, `DELETE FROM event WHERE tenant = $1`, tn)
	require.NoError(t, err)

	_, err = s.RefoldRef(ctx, tn, core.RefFromRow("ci/d1"))
	require.ErrorIs(t, err, postgres.ErrRefNotFound,
		"no events is not-found even when an orphaned row was removed")

	q := changeQuery(t, tn, time.Time{}, time.Time{}, 0, nil)
	q.Ref = "ci/d1"
	got, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)
	assert.Empty(t, got, "a ref with no events must leave no change behind")
}

// Written by hand: an extra attribute key fails the exact match.
const refoldAltered = "horopter.refold.altered=true"

// Exact match: refoldCounts reads one key and would miss others riding along.
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

func refoldErr(_ postgres.RefoldResult, err error) error { return err }

func appendStartedAndCompleted(
	ctx context.Context, t *testing.T, s *postgres.Store, tn string,
) core.Change {
	t.Helper()
	at := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
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
	return oneChange(ctx, t, s, tn, "ci/d1")
}

// alteredFields reports only the non-commutative field; first_seen_at is what operators repair.
func TestRefoldRefReportsTheFieldsItRepaired(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	want := appendStartedAndCompleted(ctx, t, s, tn)

	_, err := s.Pool().Exec(ctx, `
		UPDATE change SET title = 'wrong', first_seen_at = first_seen_at - interval '1 hour'
		WHERE tenant = $1 AND ref = 'ci/d1'`, tn)
	require.NoError(t, err)

	got, err := s.RefoldRef(ctx, tn, core.RefFromRow("ci/d1"))
	require.NoError(t, err)
	assert.Equal(t, 2, got.Events)
	assert.Equal(t, []string{"first_seen_at", "title"}, got.Altered)
	assert.Equal(t, want, oneChange(ctx, t, s, tn, "ci/d1"), "the change is repaired")
}

func TestRefoldRefOfACorrectChangeAltersNothing(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	appendStartedAndCompleted(ctx, t, s, tn)

	for range 2 {
		got, err := s.RefoldRef(ctx, tn, core.RefFromRow("ci/d1"))
		require.NoError(t, err)
		assert.Equal(t, 2, got.Events)
		assert.Equal(t, []string{}, got.Altered, "empty, and not nil: it renders as []")
	}
}

func TestRefoldRefRecreatesAMissingChange(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	want := appendStartedAndCompleted(ctx, t, s, tn)
	_, err := s.Pool().Exec(ctx, `DELETE FROM change WHERE tenant = $1 AND ref = 'ci/d1'`, tn)
	require.NoError(t, err)

	got, err := s.RefoldRef(ctx, tn, core.RefFromRow("ci/d1"))
	require.NoError(t, err)
	assert.Equal(t, []string{
		"actor", "attributes", "end", "first_seen_at", "last_event_id", "sources",
		"start", "started_at", "status", "subject", "subject_first_seen", "title",
		"title_started_at", "type",
	}, got.Altered)
	assert.Equal(t, want, oneChange(ctx, t, s, tn, "ci/d1"))
}

// Holds the tenant lock shared and the ref lock exclusively, like a mid-flight append.
func holdRefLocks(t *testing.T, s *postgres.Store, tn, ref string) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.Pool().Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	_, err = tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock_shared(hashtext($1)),
		       pg_advisory_xact_lock(hashtext($1 || '/' || $2))`, tn, ref)
	require.NoError(t, err)
}

// The re-fold shares the tenant lock with appends, unlike RebuildProjection.
func TestARefoldRunsWhileAnotherRefIsBeingAppended(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	appendStartedAndCompleted(ctx, t, s, tn)
	holdRefLocks(t, s, tn, "ci/other")

	start := time.Now()
	_, err := s.RefoldRef(ctx, tn, core.RefFromRow("ci/d1"))
	require.NoError(t, err)
	assert.Less(t, time.Since(start), postgres.AppendLockTimeout,
		"an append to another ref must not hold the re-fold up")
}

func TestARefoldWaitsForAnAppendToTheSameRef(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	appendStartedAndCompleted(ctx, t, s, tn)
	holdRefLocks(t, s, tn, "ci/d1")

	start := time.Now()
	_, err := s.RefoldRef(ctx, tn, core.RefFromRow("ci/d1"))
	require.Error(t, err, "the re-fold must wait for the append's lock and time out")
	assert.GreaterOrEqual(t, time.Since(start), postgres.AppendLockTimeout-200*time.Millisecond)
}

// Commutative fields alteredFields skips, but changedFields must name.
func TestRefoldRefReportsARepairedStartAndSources(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	want := appendStartedAndCompleted(ctx, t, s, tn)

	_, err := s.Pool().Exec(ctx, `
		UPDATE change SET started_at = started_at - interval '1 hour', sources = ARRAY['bogus']
		WHERE tenant = $1 AND ref = 'ci/d1'`, tn)
	require.NoError(t, err)

	got, err := s.RefoldRef(ctx, tn, core.RefFromRow("ci/d1"))
	require.NoError(t, err)
	assert.Equal(t, []string{"sources", "start", "started_at"}, got.Altered)
	assert.Equal(t, want, oneChange(ctx, t, s, tn, "ci/d1"))
}

func TestRefoldReportsACorruptedSubjectMap(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	ev := mkEvent(tn, eventID(t, "sfs"), "k-sfs-"+uniqueToken(), "sfs-"+uniqueToken(),
		"deployment.started", at)
	_, _, err := s.AppendEvent(ctx, ev)
	require.NoError(t, err)
	ref := core.RefFor(ev)
	_, err = s.Pool().Exec(ctx,
		`UPDATE change SET subject_first_seen = '{"wrong": 1}' WHERE tenant = $1 AND ref = $2`,
		tn, ref.String())
	require.NoError(t, err)

	res, err := s.RefoldRef(ctx, tn, ref)
	require.NoError(t, err)
	assert.Contains(t, res.Altered, "subject_first_seen")
}

func TestRefoldReportsACorruptedTitleStart(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	ev := mkEvent(tn, eventID(t, "tsa"), "k-tsa-"+uniqueToken(), "tsa-"+uniqueToken(),
		"deployment.started", at)
	ev.Title = "Deploy v2.5.0"
	_, _, err := s.AppendEvent(ctx, ev)
	require.NoError(t, err)
	ref := core.RefFor(ev)
	_, err = s.Pool().Exec(ctx, `UPDATE change SET title_started_at = NULL
		WHERE tenant = $1 AND ref = $2`, tn, ref.String())
	require.NoError(t, err)

	res, err := s.RefoldRef(ctx, tn, ref)
	require.NoError(t, err)
	assert.Contains(t, res.Altered, "title_started_at")
}
