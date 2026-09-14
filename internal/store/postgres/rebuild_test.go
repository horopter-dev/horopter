package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/store/postgres"
)

func TestRebuildReproducesProjectionExactly(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	win := postgres.ChangeQuery{
		Tenant: tn, From: at.Add(-24 * time.Hour), To: at.Add(24 * time.Hour),
	}

	events := []struct {
		suffix, idem, ref, typ string
		off                    time.Duration
	}{
		{"d1-started", "k1", "d1", "deployment.started", 0},
		{"d1-completed", "k2", "d1", "deployment.completed", 20 * time.Minute},
		{"config", "k3", "", "config.changed", 5 * time.Minute},
		{"d2-started", "k4", "d2", "deployment.started", 30 * time.Minute},
	}
	// Event ids come from eventID: event.id is a GLOBAL primary key, so literals
	// collide across tenants and across runs. eventID is backed by a monotonic
	// counter, so ULID-order replay still has a meaningful order to break.
	for _, e := range events {
		_, _, err := s.AppendEvent(ctx,
			mkEvent(tn, eventID(t, e.suffix), e.idem, e.ref, e.typ, at.Add(e.off)))
		require.NoError(t, err)
	}

	before, err := s.QueryChanges(ctx, win)
	require.NoError(t, err)
	require.Len(t, before, 3)

	require.NoError(t, s.RebuildProjection(ctx, tn))

	after, err := s.QueryChanges(ctx, win)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a rebuild must reproduce the projection exactly")
}

func TestRebuildIsTenantScoped(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	other := tn + "-keep"

	_, _, err := s.AppendEvent(ctx, mkEvent(other, eventID(t, "other-started"), "k1", "d1",
		"deployment.started", at))
	require.NoError(t, err)

	require.NoError(t, s.RebuildProjection(ctx, tn))

	got, err := s.QueryChanges(ctx, postgres.ChangeQuery{
		Tenant: other, From: at.Add(-time.Hour), To: at.Add(time.Hour),
	})
	require.NoError(t, err)
	assert.Len(t, got, 1, "rebuilding one tenant must not touch another")
}
