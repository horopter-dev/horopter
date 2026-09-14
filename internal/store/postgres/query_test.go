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

	got, err := s.QueryChanges(ctx, postgres.ChangeQuery{
		Tenant: tn, From: at.Add(-time.Hour), To: at.Add(time.Hour), Status: "failed",
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "d1", got[0].Ref)

	none, err := s.QueryChanges(ctx, postgres.ChangeQuery{
		Tenant: tn, From: at.Add(-time.Hour), To: at.Add(time.Hour), Subject: "other/stack",
	})
	require.NoError(t, err)
	assert.Empty(t, none)
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
