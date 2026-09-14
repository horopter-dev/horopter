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

	// sharedRef must be unique to this test run, not a common literal like "d1": the
	// tests in this package share one never-truncated database, and "d1" is reused by
	// many of them across many past runs, so a leak test built on that literal would be
	// corrupted by unrelated historical rows sharing the same ref — producing a false
	// pass instead of a real guarantee. eventID's counter-backed uniqueness (reused here
	// for a ref, not an id) guarantees no other test, past or future, can collide on it.
	sharedRef := eventID(t, "shared-ref")

	// tn's event shares that ref with other's event below and is given an earlier id,
	// so it sorts first if RebuildProjection's event-log read ever lost its tenant
	// filter. RebuildProjection's accumulator is keyed by ref alone, so a leaked
	// cross-tenant event with a colliding ref would fold into the SAME accumulator
	// entry as other's own event and corrupt it on upsert — even though the row count
	// stays 1. A single-event, non-colliding-ref tenant-scope test can never observe
	// that class of bug. Both events use a non-start, non-terminal type
	// ("deployment.annotated") so Start is set only by Fold's zero-value fallback:
	// whichever event folds first wins, making a leak visible as other's Start
	// silently becoming tn's earlier one.
	_, _, err := s.AppendEvent(ctx, mkEvent(tn, eventID(t, "leak"), "k0", sharedRef,
		"deployment.annotated", at.Add(-2*time.Hour)))
	require.NoError(t, err)

	_, _, err = s.AppendEvent(ctx, mkEvent(other, eventID(t, "other-annotated"), "k1", sharedRef,
		"deployment.annotated", at))
	require.NoError(t, err)

	require.NoError(t, s.RebuildProjection(ctx, tn))

	got, err := s.QueryChanges(ctx, postgres.ChangeQuery{
		Tenant: other, From: at.Add(-time.Hour), To: at.Add(time.Hour),
	})
	require.NoError(t, err)
	require.Len(t, got, 1, "rebuilding one tenant must not touch another")
	assert.True(t, got[0].Start.Equal(at),
		"other's change must keep its own Start, not tn's earlier same-ref event's — "+
			"a leaked cross-tenant read would merge them under one accumulator key")
}
