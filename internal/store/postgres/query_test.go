package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/store/postgres"
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

	got, err := s.QueryChanges(ctx, postgres.ChangeQuery{
		Tenant: tn, From: winFrom, To: winTo,
	})
	require.NoError(t, err)

	refs := map[string]bool{}
	for _, c := range got {
		refs[c.Ref] = true
	}

	assert.False(t, refs["before"], "a change entirely before the window must be excluded")
	assert.False(t, refs["after"], "a change entirely after the window must be excluded")
	assert.True(t, refs["spanning"], "a change straddling the window must be included")
	assert.True(t, refs["openearly"],
		"an open change that began before the window MUST appear — this is the bug "+
			"the predicate exists to prevent")
	assert.True(t, refs["subsecond"],
		"sub-second precision must compare temporally, not as text")
	assert.True(t, refs["onstartboundary"],
		"a change starting exactly at the window's upper edge (start_at == To) must be "+
			"included — start_at <= To is inclusive")
	assert.True(t, refs["onendboundary"],
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

	byStatus, err := s.QueryChanges(ctx, postgres.ChangeQuery{
		Tenant: tn, From: at.Add(-time.Hour), To: at.Add(time.Hour), Status: "failed",
	})
	require.NoError(t, err)
	require.Len(t, byStatus, 1, "the in_progress d2 must be excluded by the status filter")
	assert.Equal(t, "d1", byStatus[0].Ref)

	byType, err := s.QueryChanges(ctx, postgres.ChangeQuery{
		Tenant: tn, From: at.Add(-time.Hour), To: at.Add(time.Hour), Type: "deployment.started",
	})
	require.NoError(t, err)
	require.Len(t, byType, 1, "d1's type moved on to deployment.failed; only d2 must match")
	assert.Equal(t, "d2", byType[0].Ref)

	none, err := s.QueryChanges(ctx, postgres.ChangeQuery{
		Tenant: tn, From: at.Add(-time.Hour), To: at.Add(time.Hour), Subject: "other/stack",
	})
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

	combined, err := s.QueryChanges(ctx, postgres.ChangeQuery{
		Tenant: tn, From: at.Add(-time.Hour), To: at.Add(time.Hour),
		Subject: "acme/prod", Type: "deployment.completed", Status: "completed",
	})
	require.NoError(t, err)
	require.Len(t, combined, 2,
		"three filters combined must keep both completed changes and drop multi-c")
	assert.Equal(t, "multi-a", combined[0].Ref,
		"ORDER BY start_at ASC must list the earlier row first")
	assert.Equal(t, "multi-b", combined[1].Ref)

	limited, err := s.QueryChanges(ctx, postgres.ChangeQuery{
		Tenant: tn, From: at.Add(-time.Hour), To: at.Add(time.Hour),
		Subject: "acme/prod", Type: "deployment.completed", Status: "completed", Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, limited, 1, "limit must cap the combined-filter result set")
	assert.Equal(t, "multi-a", limited[0].Ref, "the earliest row by start_at must be the one kept")
}

func TestQueryChangesIsTenantScoped(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	_, _, err := s.AppendEvent(ctx, mkEvent(tn+"-other", eventID(t, "started"), "k1", "d1",
		"deployment.started", at))
	require.NoError(t, err)

	got, err := s.QueryChanges(ctx, postgres.ChangeQuery{
		Tenant: tn, From: at.Add(-time.Hour), To: at.Add(time.Hour),
	})
	require.NoError(t, err)
	assert.Empty(t, got, "queries must never cross tenants")
}
