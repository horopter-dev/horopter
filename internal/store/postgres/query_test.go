package postgres_test

import (
	"context"
	"strings"
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
	// Two boundary rows sit exactly on the window edges, so a strict < or > fails.
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
	// d2 gives the Status and Type filters a row to exclude.
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
	subjectQuery.Subjects = mustSubjects(t, "other/stack")
	none, _, err := s.QueryChanges(ctx, subjectQuery)
	require.NoError(t, err)
	assert.Empty(t, none)
}

// Parameter numbering is built incrementally, so combine every filter with Limit.
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
	// multi-c differs in type and status, so either filter drops it.
	seedCompleted("multi-a", at)
	seedCompleted("multi-b", at.Add(10*time.Minute))
	_, _, err := s.AppendEvent(ctx,
		mkEvent(tn, eventID(t, "multi-c-start"), "k-multi-c", "multi-c",
			"deployment.started", at.Add(20*time.Minute)))
	require.NoError(t, err)

	combinedQuery := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0, nil)
	combinedQuery.Subjects = mustSubjects(t, "acme/prod")
	combinedQuery.Type, combinedQuery.Status =
		"deployment.completed", "completed"
	combined, _, err := s.QueryChanges(ctx, combinedQuery)
	require.NoError(t, err)
	require.Len(t, combined, 2,
		"three filters combined must keep both completed changes and drop multi-c")
	assert.Equal(t, "ci/multi-a", combined[0].Ref.String(),
		"ORDER BY start_at ASC must list the earlier row first")
	assert.Equal(t, "ci/multi-b", combined[1].Ref.String())

	limitedQuery := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 1, nil)
	limitedQuery.Subjects = mustSubjects(t, "acme/prod")
	limitedQuery.Type, limitedQuery.Status =
		"deployment.completed", "completed"
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

// An empty status matched every overlap query until 9999; the CHECK stops it.
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

	// Every core.Statuses value: a new Status without a migration must fail here.
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

// Pins the SQL sentinel in 0002 to core.OpenSentinel.
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

// Queries core.RefFor(stored), the value a caller holds, not the bare event id.
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

// Pins the id arm's empty-correlation_ref qualifier: deleting it must fail this test.
func TestListEventsIdArmIgnoresCorrelatedEvents(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	// Unique to this run: event.id is a global primary key.
	sharedRef := eventID(t, "shared-ref")
	memberID := eventID(t, "member")

	_, _, err := s.AppendEvent(ctx,
		mkEvent(tn, memberID, "k1", sharedRef, "deployment.started", at))
	require.NoError(t, err)
	// Its own id is sharedRef, but its correlation ref puts it in another change.
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

// A bare ref matches nothing and answers empty, not an error.
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

// An accepted limit: a correlated ref equal to a point's id merges into its change.
func TestACorrelatedRefMayCollideWithAPointRef(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	pointID := eventID(t, "point")
	point, _, err := s.AppendEvent(ctx, mkEvent(tn, pointID, "k1", "", "config.changed", at))
	require.NoError(t, err)

	// Stored as ci/<pointID>, the same string RefFor gives the point.
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

// start_at = COALESCE(started_at, first_seen_at), generated by Postgres.
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

// rows and truncated are asserted as zero: the deferred block must read the named results.
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

// The zero TenantID is constructible; only the constructor refuses it.
func TestQueryConstructorsRefuseTheZeroTenant(t *testing.T) {
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

	_, err := postgres.NewChangeQuery(core.TenantID{}, at, at.Add(time.Hour), 10, nil)
	require.ErrorIs(t, err, postgres.ErrUnscopedQuery)

	_, err = postgres.NewEventQuery(core.TenantID{}, 10)
	require.ErrorIs(t, err, postgres.ErrUnscopedQuery)
}

// Readable only because stored refs go through core.RefFromRow, which does not validate.
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

// Only a refused key proves the allowlist exists.
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

func TestNewChangeQueryAcceptsAnAllowlistedAttributeKey(t *testing.T) {
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	q, err := postgres.NewChangeQuery(core.TenantIDFromRow("tnt_x"), at, at.Add(time.Hour), 10,
		map[string]string{"deployment.environment.name": "prod"})

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"deployment.environment.name": "prod"},
		postgres.AttributesOf(q))
}

// Locus is never rendered; this query is its only check.
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

// Containment, not equality: a change with two sources is found under either.
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

	// argo and flux are new names, so the filters cannot also match the fixtures above.
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

// A missing key is excluded (NULL = 'prod'); non-string values compare as ->> text.
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

// An empty value means no filter, as it does for subject, type and status.
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

type seeded struct{ locus, source, ref string }

// The ref goes through core.NewRef, the one statement of the encoding.
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

// Keyset paging resumes after (start_at, ref), so ties must come back in ref order.
func TestChangesSharingAStartAreOrderedByRef(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	for _, ref := range []string{"c", "a", "b"} {
		ev := mkEvent(tn, eventID(t, ref), "k-"+ref, ref, "deployment.started", at)
		_, _, err := s.AppendEvent(ctx, ev)
		require.NoError(t, err, "append %s", ref)
	}

	got, _, err := s.QueryChanges(ctx,
		changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0, nil))
	require.NoError(t, err)
	refs := make([]string, len(got))
	for i, c := range got {
		refs[i] = c.Ref.String()
	}
	assert.Equal(t, []string{"ci/a", "ci/b", "ci/c"}, refs)
}

func appendSubject(t *testing.T, s *postgres.Store, tn, ref, subject, typ string, at time.Time,
	lc core.Lifecycle) {
	t.Helper()
	ev := mkEvent(tn, eventID(t, ref+subject), "k-"+uniqueToken(), ref, typ, at)
	ev.Subject, ev.Lifecycle = subject, lc
	_, _, err := s.AppendEvent(context.Background(), ev)
	require.NoError(t, err)
}

func TestQueryChangesMatchesAnySubjectInScope(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	ref := "rollout-" + uniqueToken()
	appendSubject(t, s, tn, ref, "GitRepository/flux-system/flux-system", "flux.artifact.new",
		at, core.LifecycleStarted)
	appendSubject(t, s, tn, ref, "Deployment/lastlight/cloudflared",
		"flux.kustomization.applied", at.Add(5*time.Second), core.LifecycleNone)
	appendSubject(t, s, tn, ref, "Kustomization/flux-system/apps",
		"flux.kustomization.succeeded", at.Add(9*time.Second), core.LifecycleCompleted)
	appendSubject(t, s, tn, ref, "Deployment/drifted/x", "flux.kustomization.applied",
		at.Add(time.Hour), core.LifecycleNone)

	find := func(raw ...string) int {
		q := changeQuery(t, tn, at.Add(-time.Hour), at.Add(2*time.Hour), 0, nil)
		q.Subjects = mustSubjects(t, raw...)
		got, _, err := s.QueryChanges(ctx, q)
		require.NoError(t, err)
		return len(got)
	}
	assert.Equal(t, 1, find("*/lastlight/*"), "a non-last event's subject matches")
	assert.Equal(t, 0, find("*/drifted/*"), "a subject first seen after the end is out of scope")
	assert.Equal(t, 1, find("*/drifted/*", "*/lastlight/*"), "repeated patterns OR")
	assert.Equal(t, 0, find("*/monitoring/*"))
	assert.Equal(t, 1, find("Kustomization/*/*"),
		"the subject of the event that sets the end is in scope: the bound is <=, not <")

	q := changeQuery(t, tn, at.Add(-time.Hour), at.Add(2*time.Hour), 0, nil)
	q.Subjects, q.Type = mustSubjects(t, "*/lastlight/*"), "deployment.started"
	got, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)
	assert.Empty(t, got, "patterns AND with the other filters")
}

func TestQueryChangesScopeFollowsAnEarlierEnd(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 10, 6, 11, 0, 0, 0, time.UTC)
	ref := "late-" + uniqueToken()
	appendSubject(t, s, tn, ref, "Deployment/a/x", "flux.kustomization.applied",
		at.Add(5*time.Second), core.LifecycleNone)
	appendSubject(t, s, tn, ref, "Kustomization/flux-system/apps", "flux.kustomization.succeeded",
		at.Add(10*time.Second), core.LifecycleCompleted)
	q := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0, nil)
	q.Subjects = mustSubjects(t, "*/a/*")
	got, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)
	require.Len(t, got, 1, "in scope while the end is after it")

	appendSubject(t, s, tn, ref, "Kustomization/flux-system/apps", "flux.kustomization.failed",
		at.Add(2*time.Second), core.LifecycleFailed)
	got, _, err = s.QueryChanges(ctx, q)
	require.NoError(t, err)
	assert.Empty(t, got, "an older outcome moved the end before the subject; scope follows it")
}

// 1,023 stars once took Postgres 21.7s to compile; the bound is loose on purpose.
func TestQueryChangesWithAStarHeavyPatternIsCheap(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC)
	appendSubject(t, s, tn, "stars-"+uniqueToken(), "Deployment/lastlight/cloudflared",
		"flux.kustomization.applied", at, core.LifecycleNone)
	q := changeQuery(t, tn, at.Add(-time.Hour), at.Add(time.Hour), 0, nil)
	q.Subjects = mustSubjects(t, strings.Repeat("*", 1023)+"q")
	start := time.Now()
	_, _, err := s.QueryChanges(ctx, q)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 3*time.Second)
}

// The fuzzer checks Go's regexp; this checks Postgres's ~.
func TestQueryChangesMatchesMetacharacterSubjectsInPostgres(t *testing.T) {
	ctx, s := context.Background(), newStore(t)
	at := time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ subject, pattern, nearMiss string }{
		{"K/a.b/n", "K/a.b/n", "K/axb/n"},
		{"K/a+b/n", "K/a+b/n", "K/aab/n"},
		{"K/(x)/n", "K/(x)/n", "K/x/n"},
		{"K/a|b/n", "K/a|b/n", "K/a/n"},
		{"K/d]/n", "K/[[:digit:]]/n", "K/1/n"},
		{"K/=/n", "K/[=a=]/n", "K/b/n"},
		{"K/x$y/n", "K/x$y/n", "K/x/n"},
	} {
		t.Run(tc.subject, func(t *testing.T) {
			tn := tenant(t) // one per case, so no case's near miss is another's match
			ref := "meta-" + uniqueToken()
			appendSubject(t, s, tn, ref, tc.subject, "x.annotated", at, core.LifecycleNone)
			appendSubject(t, s, tn, ref+"-miss", tc.nearMiss, "x.annotated", at,
				core.LifecycleNone)
			q := changeQuery(t, tn, at.Add(-time.Minute), at.Add(time.Minute), 0, nil)
			q.Subjects = mustSubjects(t, tc.pattern)
			got, _, err := s.QueryChanges(ctx, q)
			require.NoError(t, err)
			var refs []string
			for _, c := range got {
				refs = append(refs, c.Ref.String())
			}
			assert.Equal(t, []string{"ci/" + ref}, refs, "%q ~ %q in Postgres", tc.pattern, tc.subject)
		})
	}
}
