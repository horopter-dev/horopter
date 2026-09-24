package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/store/postgres"
)

func TestOverlapPredicate(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	base := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	winFrom, winTo := base, base.Add(time.Hour)

	seed := func(ref string, start time.Time, end *time.Time) {
		_, _, err := s.AppendEvent(ctx,
			mkEvent(tn, eventID(t, "start-"+ref), "k"+ref, ref, "deployment.started", start))
		require.NoError(t, err)
		if end != nil {
			_, _, err = s.AppendEvent(ctx,
				mkEvent(tn, eventID(t, "end-"+ref), "j"+ref, ref, "deployment.completed", *end))
			require.NoError(t, err)
		}
	}
	at := func(d time.Duration) *time.Time { v := base.Add(d); return &v }

	seed("before", base.Add(-3*time.Hour), at(-2*time.Hour))
	seed("after", base.Add(2*time.Hour), at(3*time.Hour))
	seed("spanning", base.Add(-time.Hour), at(2*time.Hour))
	seed("openearly", base.Add(-5*time.Hour), nil)
	seed("subsecond", base.Add(30*time.Minute+500*time.Millisecond), at(31*time.Minute))
	// Boundary cases: everything above sits an hour or more clear of the window edges,
	// so an inclusive predicate regressing to strict < / > would pass unnoticed. These two
	// sit exactly on each edge.
	seed("onstartboundary", winTo, nil) // start_at == To exactly; open, so end_effective is far future
	seed("onendboundary", base.Add(-2*time.Hour), at(0))
	// at(0) == winFrom exactly, so end_effective == From exactly.

	got, _, err := s.QueryChanges(ctx,
		changeQuery(t, tn, winFrom, winTo, 0, nil))
	require.NoError(t, err)

	refs := map[string]bool{}
	for _, c := range got {
		refs[c.Ref.String()] = true
	}

	assert.False(t, refs["ci/before"], "a change entirely before the window must be excluded")
	assert.False(t, refs["ci/after"], "a change entirely after the window must be excluded")
	assert.True(t, refs["ci/spanning"], "a change straddling the window must be included")
	assert.True(t, refs["ci/openearly"],
		"an open change that began before the window MUST appear — this is the bug "+
			"the predicate exists to prevent")
	assert.True(t, refs["ci/subsecond"],
		"sub-second precision must compare temporally, not as text")
	assert.True(t, refs["ci/onstartboundary"],
		"a change starting exactly at the window's upper edge (start_at == To) must be "+
			"included — start_at <= To is inclusive")
	assert.True(t, refs["ci/onendboundary"],
		"a change ending exactly at the window's lower edge (end_effective == From) must "+
			"be included — end_effective >= From is inclusive")
}

func TestQueryChangesFiltersBySubjectTypeAndStatus(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	_, _, err := s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "started"), "k1", "d1", "deployment.started", at))
	require.NoError(t, err)
	_, _, err = s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "failed"), "k2", "d1", "deployment.failed", at.Add(time.Minute)))
	require.NoError(t, err)
	// d2 stays in_progress, type deployment.started — it exists so the Status and Type
	// filters below have a row to exclude, not just one row that is trivially the only
	// candidate. Without d2, require.Len(t, got, 1) below would pass identically with the
	// filter's SQL clause deleted entirely.
	_, _, err = s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "d2-started"), "k3", "d2", "deployment.started", at))
	require.NoError(t, err)

	statusQuery := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0, nil)
	statusQuery.Status = "failed"
	byStatus, _, err := s.QueryChanges(ctx, statusQuery)
	require.NoError(t, err)
	require.Len(t, byStatus, 1, "the in_progress d2 must be excluded by the status filter")
	assert.Equal(t, "ci/d1", byStatus[0].Ref.String())

	typeQuery := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0, nil)
	typeQuery.Type = "deployment.started"
	byType, _, err := s.QueryChanges(ctx, typeQuery)
	require.NoError(t, err)
	require.Len(t, byType, 1, "d1's type moved on to deployment.failed; only d2 must match")
	assert.Equal(t, "ci/d2", byType[0].Ref.String())

	subjectQuery := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0, nil)
	subjectQuery.Subject = "other/stack"
	none, _, err := s.QueryChanges(ctx, subjectQuery)
	require.NoError(t, err)
	assert.Empty(t, none)
}

// TestQueryChangesCombinesFiltersAndLimit exercises Subject, Type, and Status active
// together, and the same three combined with Limit — the positional parameter numbering
// ($4, $5, $6, $7) is only ever built incrementally by QueryChanges, so this is the
// highest-risk case for an off-by-one that a single-filter test cannot expose.
func TestQueryChangesCombinesFiltersAndLimit(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	seedCompleted := func(ref string, start time.Time) {
		_, _, err := s.AppendEvent(ctx,
			mkEvent(tn, eventID(t, ref+"-start"), "k-"+ref+"-1", ref, "deployment.started", start))
		require.NoError(t, err)
		_, _, err = s.AppendEvent(ctx,
			mkEvent(tn, eventID(t, ref+"-done"), "k-"+ref+"-2", ref, "deployment.completed",
				start.Add(time.Minute)))
		require.NoError(t, err)
	}
	// multi-a and multi-b share type "deployment.completed" and status "completed", with
	// distinct start times so ORDER BY start_at ASC is deterministic. multi-c differs in
	// both type and status, so it must drop out whenever either filter is applied.
	seedCompleted("multi-a", at)
	seedCompleted("multi-b", at.Add(10*time.Minute))
	_, _, err := s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "multi-c-start"), "k-multi-c", "multi-c",
			"deployment.started", at.Add(20*time.Minute)))
	require.NoError(t, err)

	combinedQuery := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0, nil)
	combinedQuery.Subject, combinedQuery.Type, combinedQuery.Status =
		"acme/prod", "deployment.completed", "completed"
	combined, _, err := s.QueryChanges(ctx, combinedQuery)
	require.NoError(t, err)
	require.Len(t, combined, 2,
		"three filters combined must keep both completed changes and drop multi-c")
	assert.Equal(t, "ci/multi-a", combined[0].Ref.String(),
		"ORDER BY start_at ASC must list the earlier row first")
	assert.Equal(t, "ci/multi-b", combined[1].Ref.String())

	limitedQuery := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 1, nil)
	limitedQuery.Subject, limitedQuery.Type, limitedQuery.Status =
		"acme/prod", "deployment.completed", "completed"
	limited, _, err := s.QueryChanges(ctx, limitedQuery)
	require.NoError(t, err)
	require.Len(t, limited, 1, "limit must cap the combined-filter result set")
	assert.Equal(t, "ci/multi-a", limited[0].Ref.String(),
		"the earliest row by start_at must be the one kept")
}

func TestQueryChangesIsTenantScoped(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	other := siblingTenant(t, tn, "other")
	_, _, err := s.AppendEvent(ctx, mkEvent(other, eventID(t, "started"), "k1", "d1",
		"deployment.started", at))
	require.NoError(t, err)

	got, _, err := s.QueryChanges(ctx,
		changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0, nil))
	require.NoError(t, err)
	assert.Empty(t, got, "queries must never cross tenants")
}

// TestChangeStatusVocabularyIsEnforced proves the CHECK migration 0002 adds is real. An
// empty status made a change both unqueryable — QueryChanges reads an empty Status as
// "no filter" — and, with end_at NULL, an open interval matching every overlap query
// until the year 9999. The constraint is what stops that class of row ever being written
// again, whatever a future code path forgets.
func TestChangeStatusVocabularyIsEnforced(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	for _, bad := range []string{"", "in-progress", "IN_PROGRESS", "pending"} {
		t.Run("status="+bad, func(t *testing.T) {
			_, err := s.Pool().Exec(ctx, `
				INSERT INTO change (tenant, ref, subject, type, title, actor, started_at,
				                    first_seen_at, end_at, status, attributes, last_event_id)
				VALUES ($1,$2,'acme/prod','deployment.annotated','','ci-bot',NULL,$3,NULL,
				        $4,'{}'::jsonb,'seed')`,
				tn, "ci/"+eventID(t, "bad-status"), at, bad)
			require.Error(t, err, "a status outside internal/core's vocabulary must be rejected")
			assert.Contains(t, err.Error(), "change_status_vocabulary")
		})
	}

	// Every value in core.Statuses, not one sample of it. This half of the test used to
	// insert 'in_progress' alone while asserting it proved the constraint "must still
	// admit every status internal/core can produce" — a claim one hardcoded literal
	// cannot support. Adding a seventh Status without a migration compiled, passed here,
	// and failed at runtime on the first insert.
	for _, st := range core.Statuses {
		t.Run("admits "+string(st), func(t *testing.T) {
			var valid int
			require.NoError(t, s.Pool().QueryRow(ctx, `
				INSERT INTO change (tenant, ref, subject, type, title, actor, started_at,
				                    first_seen_at, end_at, status, attributes, last_event_id)
				VALUES ($1,$2,'acme/prod','deployment.annotated','','ci-bot',NULL,$3,NULL,
				        $4,'{}'::jsonb,'seed')
				RETURNING 1`,
				tn, "ci/"+eventID(t, "good-"+string(st)), at, string(st)).Scan(&valid),
				"migration 0002's CHECK must admit every value in core.Statuses; %q is in "+
					"the Go vocabulary and not in the SQL one", st)
		})
	}
}

// TestEndEffectiveIsGeneratedFromEndAt pins the one remaining copy of the open-change
// sentinel. end_effective is the overlap index's key; it used to be computed in Go and
// written alongside end_at, so the two could drift and the index would answer overlap
// queries from a value no longer implied by the data. Postgres owns it now, and this
// asserts its generated value against core.OpenSentinel so the SQL literal in 0002 and
// the Go constant cannot diverge unnoticed.
func TestEndEffectiveIsGeneratedFromEndAt(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	endEffective := func(ref string) time.Time {
		var v time.Time
		require.NoError(t, s.Pool().QueryRow(ctx,
			`SELECT end_effective FROM change WHERE tenant = $1 AND ref = $2`, tn, ref).Scan(&v))
		return v
	}

	_, _, err := s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "start"), "k1", "gen-1", "deployment.started", at))
	require.NoError(t, err)
	assert.True(t, endEffective("ci/gen-1").Equal(core.OpenSentinel),
		"an open change's end_effective must be core.OpenSentinel, generated from a NULL end_at")

	done := at.Add(20 * time.Minute)
	_, _, err = s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "done"), "k2", "gen-1", "deployment.completed", done))
	require.NoError(t, err)
	assert.True(t, endEffective("ci/gen-1").Equal(done),
		"closing a change must move end_effective to end_at without Go writing it")
}

// TestListEventsFindsAPointChangesEvents covers the ref of a change with no correlation
// ref. core.RefFor makes such a change's ref its locus and its own event id, so filtering
// the event log on correlation_ref alone returned nothing for every point change the read
// side serves.
//
// It queries core.RefFor(stored) rather than the bare event id deliberately. Asserting on
// the id is what let this test keep passing when 0006 moved the ref: the value a caller
// actually holds comes from a change row or from the append response, both of which are
// RefFor's output, and that is the only value worth pinning.
func TestListEventsFindsAPointChangesEvents(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	pointID := eventID(t, "point")
	stored, _, err := s.AppendEvent(ctx, mkEvent(tn, pointID, "k1", "", "config.changed", at))
	require.NoError(t, err)
	pointRef := core.RefFor(stored).String()
	require.Equal(t, "ci/"+pointID, pointRef, "precondition: a point ref carries its locus")

	_, _, err = s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "correlated"), "k2", "dep-9", "deployment.started", at))
	require.NoError(t, err)

	point, _, err := s.ListEvents(ctx, refQuery(t, tn, pointRef))
	require.NoError(t, err)
	require.Len(t, point, 1, "a point change's ref is its locus and its own event id, and "+
		"must list that event")
	assert.Equal(t, pointID, point[0].ID)

	correlated, _, err := s.ListEvents(ctx, refQuery(t, tn, "ci/dep-9"))
	require.NoError(t, err)
	require.Len(t, correlated, 1, "a correlated ref must still list only its own events")
	assert.Equal(t, "ci/dep-9", correlated[0].CorrelationRef.String())
}

// TestListEventsIdArmIgnoresCorrelatedEvents pins that listing a ref returns only the
// events belonging to it, and not a second change's event whose own id equals the ref's
// id half.
//
// What it pins is this qualifier in ListEvents' id arm:
//
//	correlation_ref = ''
//
// It is written in a code block because gofmt rewrites two apostrophes into a closing
// smart quote in running doc-comment text, which is how this comment came to spell a SQL
// empty-string literal as a character SQL has no use for.
//
// Without the qualifier the id arm is "locus = <locus> AND id = <id>", which also matches
// an event whose own id happens to equal the ref's id half — and since a correlated
// event's id is an arbitrary ULID while a ref is producer-supplied, a producer using event
// ids as correlation refs makes exactly that collision. The two events below belong to
// different changes and the audit log for one must not show the other's.
//
// This guard is only as reachable as the id arm makes it, and it has been unreachable
// before: between 0006 and the ref split, the id arm compared the WHOLE ref
// against e.id, no event id contains a '/', and the decoy below was excluded by "id <>
// ref" alone — so the qualifier could have been deleted with this test staying green.
// Check it by mutation rather than by its colour: deleting the qualifier from
// eventSelection must make this test FAIL.
func TestListEventsIdArmIgnoresCorrelatedEvents(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	// sharedRef is both a correlation ref (for memberID below) and another event's id.
	// It must be unique to this run: event.id is a global primary key across every tenant
	// and every past execution.
	sharedRef := eventID(t, "shared-ref")
	memberID := eventID(t, "member")

	_, _, err := s.AppendEvent(ctx,
		mkEvent(tn, memberID, "k1", sharedRef, "deployment.started", at))
	require.NoError(t, err)
	// This event's own id IS sharedRef, but it carries a correlation ref of its own, so
	// it belongs to that other change and not to sharedRef's.
	_, _, err = s.AppendEvent(ctx,
		mkEvent(tn, sharedRef, "k2", eventID(t, "other-ref"), "deployment.started", at))
	require.NoError(t, err)

	got, _, err := s.ListEvents(ctx, refQuery(t, tn, "ci/"+sharedRef))
	require.NoError(t, err)
	require.Len(t, got, 1,
		"the id arm must only ever match the uncorrelated point that owns the ref; "+
			"matching a correlated event whose id equals the ref leaks another change's log")
	assert.Equal(t, memberID, got[0].ID)
}

// TestListEventsIgnoresARefWithNoLocus pins what eventSelection does with a ref that does
// not parse. After 0006 no ref is bare, so a bare one names nothing and must match nothing
// — including the point event whose id it happens to equal. core.ParseRef refuses it and
// the empty locus bound in its place matches no row, because event_locus_shape forbids
// one. Answering 200 with an empty list, rather than erroring, is what this pins.
func TestListEventsIgnoresARefWithNoLocus(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	pointID := eventID(t, "point")
	_, _, err := s.AppendEvent(ctx, mkEvent(tn, pointID, "k1", "", "config.changed", at))
	require.NoError(t, err)

	evs, _, err := s.ListEvents(ctx, refQuery(t, tn, pointID))
	require.NoError(t, err)
	assert.Empty(t, evs, "a bare event id is not a ref after 0006 and must match nothing")
}

// TestListEventsIgnoresARefWithAnEmptyLocus pins the read half of the rule
// change_ref_has_locus enforces on the write half: '/x' carries a slash but no locus, and
// an empty locus is not a locus. event_locus_shape means no row can hold one, so the
// predicate matches nothing — but only as long as the locus arm is compared, not assumed.
func TestListEventsIgnoresARefWithAnEmptyLocus(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	pointID := eventID(t, "point")
	_, _, err := s.AppendEvent(ctx, mkEvent(tn, pointID, "k1", "", "config.changed", at))
	require.NoError(t, err)

	evs, _, err := s.ListEvents(ctx, refQuery(t, tn, "/"+pointID))
	require.NoError(t, err)
	assert.Empty(t, evs, "an empty locus is not a locus, so /<id> names nothing")
}

// TestACorrelatedRefMayCollideWithAPointRef pins the limit the design accepts rather than
// guards, recorded under "Named limits" in the spec. Before 0006 the two shapes could not
// collide: a correlated ref was <locus>/<ref> and a point ref was a bare ULID. Both are now
// <locus>/<something>, so a producer that posts a correlation_ref equal to a point event's
// own id, in the same tenant and locus, writes into that point's change row.
//
// It is consistent rather than corrupting — events are immutable and stored separately, so
// a rebuild reproduces the same merge — and reaching it takes a deliberate choice by a
// credential that can already append to the tenant. Guarding it would mean refusing every
// ULID-shaped correlation ref, which refuses producers whose build or trace ids are ULIDs.
//
// The merge is asserted where it is observable: the shared ref lists BOTH events, each
// reaching the result through a different arm of eventSelection's predicate — the point
// through the id arm, the correlated event through correlation_ref — and the change row's
// last_event_id names the correlated event, so it demonstrably folded into the point's row
// rather than merely sharing a name with it.
//
// Counting change rows for the ref cannot show any of that. change is PRIMARY KEY (tenant,
// ref), so the count can only be 0 or 1, and the precondition below already establishes
// that the point alone creates that row: a count of 1 is entailed before the correlated
// event is appended, and would read the same if the collision never happened.
func TestACorrelatedRefMayCollideWithAPointRef(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	pointID := eventID(t, "point")
	point, _, err := s.AppendEvent(ctx, mkEvent(tn, pointID, "k1", "", "config.changed", at))
	require.NoError(t, err)

	// The producer posts the point's own id as its correlation ref. mkEvent composes the
	// locus onto it exactly as ingress's resolveRef would, so this is stored as
	// "ci/<pointID>" — the same string RefFor gives the point.
	corrID := eventID(t, "corr")
	_, _, err = s.AppendEvent(ctx,
		mkEvent(tn, corrID, "k2", pointID, "deployment.started", at))
	require.NoError(t, err)

	sharedRef := core.RefFor(point).String()
	require.Equal(t, "ci/"+pointID, sharedRef,
		"precondition: the point's ref is what the correlated event claimed")

	evs, _, err := s.ListEvents(ctx, refQuery(t, tn, sharedRef))
	require.NoError(t, err)
	require.Len(t, evs, 2, "the shared ref must list both events; this is the collision "+
		"itself, not a side effect of it")
	assert.Equal(t, []string{pointID, corrID}, []string{evs[0].ID, evs[1].ID},
		"the point arrives through the id arm and the correlated event through "+
			"correlation_ref, in id order")

	var lastEventID string
	require.NoError(t, s.Pool().QueryRow(ctx,
		`SELECT last_event_id FROM change WHERE tenant = $1 AND ref = $2`,
		tn, sharedRef).Scan(&lastEventID))
	assert.Equal(t, corrID, lastEventID,
		"the correlated event folded into the point's own change row; this is the "+
			"accepted limit, not a bug")
}

// TestQueryChangesFindsAPointChangeByItsPrefixedRef covers the OTHER read route. Nothing in
// this change touches changeSelection, which matches c.ref directly, so a point change's
// migrated ref must reach it unchanged — and no test would say otherwise if it stopped.
func TestQueryChangesFindsAPointChangeByItsPrefixedRef(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	pointID := eventID(t, "point")
	stored, _, err := s.AppendEvent(ctx, mkEvent(tn, pointID, "k1", "", "config.changed", at))
	require.NoError(t, err)

	q := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 10, nil)
	q.Ref = core.RefFor(stored).String()
	changes, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)
	require.Len(t, changes, 1, "a point change must be reachable by its prefixed ref")
	assert.Equal(t, "ci/"+pointID, changes[0].Ref.String())
}

// TestStartAtIsGeneratedFromTheTwoStartFacts is TestEndEffectiveIsGeneratedFromEndAt's
// counterpart for migration 0003. start_at is no longer written by Go: Postgres derives
// it from COALESCE(started_at, first_seen_at), so the precedence of the assertion over
// the inference is structural rather than a branch a later edit can get wrong. This
// asserts the derivation against a ref whose only events are non-start (started_at stays
// NULL, so start_at must follow first_seen_at) and then against the same ref once a start
// event asserts a LATER time — under a single start column that assertion would have been
// min'd away or would have destroyed the earlier evidence.
func TestStartAtIsGeneratedFromTheTwoStartFacts(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	type row struct {
		startAt     time.Time
		startedAt   *time.Time
		firstSeenAt time.Time
	}
	read := func(ref string) row {
		var r row
		require.NoError(t, s.Pool().QueryRow(ctx,
			`SELECT start_at, started_at, first_seen_at FROM change
			 WHERE tenant = $1 AND ref = $2`, tn, ref).Scan(&r.startAt, &r.startedAt, &r.firstSeenAt))
		return r
	}

	_, _, err := s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "annotated"), "k1", "gen-start", "deployment.annotated", at))
	require.NoError(t, err)

	inferred := read("ci/gen-start")
	assert.Nil(t, inferred.startedAt, "no producer has asserted a start yet")
	assert.True(t, inferred.firstSeenAt.Equal(at))
	assert.True(t, inferred.startAt.Equal(at),
		"with no assertion, start_at must fall through to the inference")

	asserted := at.Add(20 * time.Minute)
	_, _, err = s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "started"), "k2", "gen-start", "deployment.started", asserted))
	require.NoError(t, err)

	got := read("ci/gen-start")
	require.NotNil(t, got.startedAt)
	assert.True(t, got.startedAt.Equal(asserted))
	assert.True(t, got.firstSeenAt.Equal(at),
		"the earlier evidence must survive the assertion rather than be overwritten")
	assert.True(t, got.startAt.Equal(asserted),
		"once asserted, start_at must follow the assertion even though it is later than "+
			"the earliest event seen")
}

// TestFailedQueryChangesStillRecordsItsAttributes is the failure half of the read path's
// instrumentation. The attributes that say what was asked for — tenant, limit and window
// — are all known before the query runs, so recording them only after it succeeded left
// every failed read describing nothing at all, which is the one case anyone reads a span
// for. A cancelled context is the cheapest deterministic way to make the pool refuse.
//
// rows and truncated are asserted as their zero values rather than skipped: they come
// from the named results, and asserting them pins that the deferred block reads the
// results rather than stale locals.
func TestFailedQueryChangesStillRecordsItsAttributes(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()

	s, tn := newStore(t), tenant(t)
	from := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	to := from.Add(90 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := s.QueryChanges(ctx,
		changeQuery(t, tn, from, to, 25, nil))
	require.Error(t, err, "a cancelled context must fail the query, not return an empty result")

	assert.Equal(t, []string{tn},
		spanAttrValues(rec, "QueryChanges", "horopter.tenant"),
		"a failed query must still say which tenant asked")
	assert.Equal(t, []string{"25"},
		spanAttrValues(rec, "QueryChanges", "horopter.query.limit"))
	assert.Equal(t, []string{"5400000"},
		spanAttrValues(rec, "QueryChanges", "horopter.query.window_ms"),
		"the window is known at entry; a failing query is when its width matters most")
	assert.Equal(t, []string{"0"}, spanAttrValues(rec, "QueryChanges", "horopter.query.rows"))
	assert.Equal(t, []string{"false"},
		spanAttrValues(rec, "QueryChanges", "horopter.query.truncated"))
}

// TestFailedListEventsStillRecordsItsAttributes is the same guard on the other read
// method, which has its own copy of the deferred block.
func TestFailedListEventsStillRecordsItsAttributes(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()

	s, tn := newStore(t), tenant(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := s.ListEvents(ctx, eventQuery(t, tn, 7))
	require.Error(t, err)

	assert.Equal(t, []string{tn}, spanAttrValues(rec, "ListEvents", "horopter.tenant"))
	assert.Equal(t, []string{"7"}, spanAttrValues(rec, "ListEvents", "horopter.query.limit"))
	assert.Equal(t, []string{"0"}, spanAttrValues(rec, "ListEvents", "horopter.query.rows"))
}

// TestQueryConstructorsRefuseTheZeroTenant is the runtime half of the tenant gate the spec
// promises: "the zero value is caught by one guard inside the constructor".
//
// The compile-time half — TenantID's unexported field — makes an unscoped query
// unwritable as a struct literal, but the zero value is still constructible, and a query
// carrying one selects the empty tenant and returns an empty result set indistinguishable
// from a legitimate one. Nothing reaches this today because egress's admit resolves every
// read's tenant first; a route registered without it would compile.
func TestQueryConstructorsRefuseTheZeroTenant(t *testing.T) {
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

	_, err := postgres.NewChangeQuery(core.TenantID{}, at, at.Add(time.Hour), 10, nil)
	require.ErrorIs(t, err, postgres.ErrUnscopedQuery)

	_, err = postgres.NewEventQuery(core.TenantID{}, 10)
	require.ErrorIs(t, err, postgres.ErrUnscopedQuery)
}

// core.ParseRef is deliberately stricter than change_ref_has_locus, which admits a ref whose
// producer half is empty. That asymmetry is only safe because stored values are read through
// core.RefFromRow, which does not validate. A scan that used ParseRef would make this row
// unreadable.
func TestAStoredRefTheParserWouldRejectIsStillReadable(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	// Written directly: ingest cannot produce an empty producer ref, and ParseRef refuses one.
	_, err := s.Pool().Exec(ctx, `INSERT INTO change
		(tenant, ref, subject, type, title, actor, started_at, first_seen_at, end_at,
		 status, attributes, last_event_id)
		VALUES ($1, 'ci/', 'acme/prod', 'deployment.started', '', '', $2, $2, NULL,
		        'in_progress', '{}'::jsonb, 'e0')`, tn, at)
	require.NoError(t, err, "precondition: change_ref_has_locus accepts a ref like ci/")

	q := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 10, nil)
	got, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)
	require.Len(t, got, 1, "a legally stored ref must survive a read")
	assert.Equal(t, "ci/", got[0].Ref.String(), "and must round-trip unchanged")
}

// TestNewChangeQueryRefusesAnUnfilterableAttributeKey is the discriminating assertion for
// the allowlist. Asserting that an ALLOWLISTED key filters correctly passes whether or not
// the allowlist is enforced at all; only a rejected key proves it exists.
//
// A caller filtering on an unindexed key and receiving every row has been told the wrong
// answer, which is why this is a refusal rather than a silently dropped clause — the same
// conclusion Area A reached for --subject-pattern.
func TestNewChangeQueryRefusesAnUnfilterableAttributeKey(t *testing.T) {
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	_, err := postgres.NewChangeQuery(core.TenantIDFromRow("tnt_x"), at, at.Add(time.Hour), 10,
		map[string]string{"service.version": "2.5.0"})

	require.Error(t, err)
	assert.ErrorIs(t, err, postgres.ErrUnfilterableAttribute)
	assert.Contains(t, err.Error(), "service.version",
		"the error must name the offending key, or a caller cannot act on it")
	assert.Contains(t, err.Error(), "deployment.environment.name",
		"and must list what IS filterable, or the caller has to guess")
}

// TestNewChangeQueryAcceptsAnAllowlistedAttributeKey is the other half: the allowlist must
// not refuse everything.
func TestNewChangeQueryAcceptsAnAllowlistedAttributeKey(t *testing.T) {
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	q, err := postgres.NewChangeQuery(core.TenantIDFromRow("tnt_x"), at, at.Add(time.Hour), 10,
		map[string]string{"deployment.environment.name": "prod"})

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"deployment.environment.name": "prod"},
		postgres.AttributesOf(q))
}

// TestQueryChangesFiltersByLocus pins the generated column through the only surface it is
// observable from. The locus is never rendered, so if split_part is wrong, or the column is
// missing, or the filter names the wrong column, this query is what says so.
func TestQueryChangesFiltersByLocus(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	appendUnder(t, s, tn, seeded{locus: "ci", source: "github", ref: "dep-ci"}, at)
	appendUnder(t, s, tn, seeded{locus: "gha", source: "github", ref: "dep-gha"}, at)

	q := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0, nil)
	q.Locus = "gha"
	got, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, "gha/dep-gha", got[0].Ref.String())
}

// TestQueryChangesFiltersBySource pins the column end to end: an event's source reaches the
// change through the fold, comes back out as a member of Sources, and the filter matches by
// containment rather than equality — which is what lets a change with two sources be found
// under either.
func TestQueryChangesFiltersBySource(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	appendUnder(t, s, tn, seeded{locus: "ci", source: "buildkite", ref: "dep-bk"}, at)
	appendUnder(t, s, tn, seeded{locus: "ci", source: "github", ref: "dep-gh"}, at)

	q := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0, nil)
	q.Source = "github"
	got, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, "ci/dep-gh", got[0].Ref.String())
	assert.Equal(t, []string{"github"}, got[0].Sources, "and the value must come back on the change")

	// A change both systems reported is found under either, which is the whole reason
	// sources is a set: under last-wins it was findable only under whichever reported last.
	// argo and flux are new names, not reused from the fixtures above, so the two source
	// filters below cannot accidentally also match dep-gh or dep-bk.
	argoEvent := mkEvent(tn, eventID(t, "dep-both-1"), "k-dep-both-1", "dep-both",
		"deployment.started", at)
	argoEvent.Source = "argo"
	_, _, err = s.AppendEvent(ctx, argoEvent)
	require.NoError(t, err)
	fluxEvent := mkEvent(tn, eventID(t, "dep-both-2"), "k-dep-both-2", "dep-both",
		"deployment.completed", at.Add(time.Minute))
	fluxEvent.Source = "flux"
	_, _, err = s.AppendEvent(ctx, fluxEvent)
	require.NoError(t, err)

	for _, src := range []string{"argo", "flux"} {
		bothQuery := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0, nil)
		bothQuery.Source = src
		got, _, err := s.QueryChanges(ctx, bothQuery)
		require.NoError(t, err)
		require.Len(t, got, 1, "source %q must find the two-source change", src)
		assert.Equal(t, "ci/dep-both", got[0].Ref.String())
		assert.ElementsMatch(t, []string{"argo", "flux"}, got[0].Sources,
			"the change genuinely carries both sources, not just the one filtered on")
	}
}

// TestQueryChangesFiltersByAttribute covers the allowlisted key, and with it the two SQL
// behaviours the spec leans on without naming: a change that lacks the key entirely is
// excluded (attributes ->> 'k' is NULL, and NULL = 'prod' is NULL rather than false), and a
// non-string JSON value is compared as the text ->> renders it.
func TestQueryChangesFiltersByAttribute(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	withAttrs := func(ref string, attrs map[string]any) {
		ev := mkEvent(tn, eventID(t, ref), "k-"+ref, ref, "deployment.started", at)
		ev.Attributes = attrs
		_, _, err := s.AppendEvent(ctx, ev)
		require.NoError(t, err)
	}
	withAttrs("dep-prod", map[string]any{"deployment.environment.name": "prod"})
	withAttrs("dep-stg", map[string]any{"deployment.environment.name": "staging"})
	withAttrs("dep-none", map[string]any{"service.version": "2.5.0"})
	withAttrs("dep-num", map[string]any{"deployment.environment.name": 5})

	byEnv := func(v string) []core.Change {
		q := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0,
			map[string]string{"deployment.environment.name": v})
		got, _, err := s.QueryChanges(ctx, q)
		require.NoError(t, err)
		return got
	}

	prod := byEnv("prod")
	require.Len(t, prod, 1, "a change without the key at all must be excluded, not matched "+
		"by SQL NULL semantics falling through")
	assert.Equal(t, "ci/dep-prod", prod[0].Ref.String())

	num := byEnv("5")
	require.Len(t, num, 1,
		"attributes is JSONB and ->> renders a number as text, so a numeric value is "+
			"matchable and matches by its rendering")
	assert.Equal(t, "ci/dep-num", num[0].Ref.String())
}

// TestQueryChangesTreatsAnEmptyAttributeValueAsNoFilter settles the one genuine ambiguity in
// the design: ?attr.deployment.environment.name= could mean "where the value is the empty
// string" or "no filter". It means no filter, because subject, type and status all already
// mean that, and a caller who omits a value has not asked a narrower question.
func TestQueryChangesTreatsAnEmptyAttributeValueAsNoFilter(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	appendUnder(t, s, tn, seeded{locus: "ci", source: "github", ref: "dep-a"}, at)
	appendUnder(t, s, tn, seeded{locus: "ci", source: "github", ref: "dep-b"}, at)

	q := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0,
		map[string]string{"deployment.environment.name": ""})
	got, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)

	assert.Len(t, got, 2, "an empty value must not filter, and must not filter for \"\"")
}

// seeded is one event to append, named rather than positional: locus, source and ref are
// three adjacent strings, and a transposition between them is exactly what issueFlags made
// a compile error elsewhere in this tree.
type seeded struct{ locus, source, ref string }

// appendUnder appends one event under a chosen locus and source, which mkEvent hardcodes
// both to "ci". The correlation ref is composed through core.NewRef so the fixture depends
// on the one statement of the encoding, as production does.
func appendUnder(t *testing.T, s *postgres.Store, tn string, sd seeded, at time.Time) {
	t.Helper()
	ev := mkEvent(tn, eventID(t, sd.ref), "k-"+sd.ref, sd.ref, "deployment.started", at)
	ev.Locus = core.LocusFromRow(sd.locus)
	corr, err := core.NewRef(ev.Locus, sd.ref)
	require.NoError(t, err)
	ev.CorrelationRef = corr
	ev.Source = sd.source
	_, _, err = s.AppendEvent(context.Background(), ev)
	require.NoError(t, err)
}
