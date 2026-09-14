package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/core"
	"github.com/yo61/horopter/internal/store/postgres"
)

func newStore(t *testing.T) *postgres.Store {
	t.Helper()
	ctx := context.Background()
	s, err := postgres.Open(ctx, dsn(t))
	require.NoError(t, err)
	t.Cleanup(s.Close)
	// Each test owns its own tenant, so tests are isolated without truncating
	// shared tables and can run in parallel.
	return s
}

// tenant returns a per-test tenant id, keeping tests isolated from one another.
func tenant(t *testing.T) string {
	t.Helper()
	return "test-" + t.Name()
}

func mkEvent(tn, id, idem, corr, typ string, at time.Time) core.Event {
	return core.Event{
		ID: id, Tenant: tn, IdempotencyKey: idem, SchemaVersion: 1,
		Source: "ci", Type: typ, Subject: "acme/prod", Actor: "ci-bot",
		OccurredAt: at, ReceivedAt: at, CorrelationRef: corr,
		Attributes: map[string]any{"service.version": "2.5.0"},
	}
}

func TestAppendEventStoresAndProjects(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

	_, created, err := s.AppendEvent(ctx, mkEvent(tn, "01A", "k1", "dep-7", "deployment.started", at))
	require.NoError(t, err)
	assert.True(t, created)

	var ref, status string
	var endAt *time.Time
	err = s.Pool().QueryRow(ctx,
		`SELECT ref, status, end_at FROM change WHERE tenant = $1 AND ref = $2`,
		tn, "dep-7").Scan(&ref, &status, &endAt)
	require.NoError(t, err)
	assert.Equal(t, "dep-7", ref)
	assert.Equal(t, "in_progress", status)
	assert.Nil(t, endAt, "a started-but-unfinished change must have a NULL end")
}

func TestAppendEventIsIdempotent(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	e := mkEvent(tn, "01B", "build-42", "dep-7", "deployment.started", at)

	first, created, err := s.AppendEvent(ctx, e)
	require.NoError(t, err)
	require.True(t, created)

	dup := e
	dup.ID = "01Z" // a retry generates a new id but reuses the key
	second, created, err := s.AppendEvent(ctx, dup)
	require.NoError(t, err)
	assert.False(t, created, "a duplicate idempotency key must not create a second event")
	assert.Equal(t, first.ID, second.ID, "the originally stored event must be returned")

	evs, err := s.ListEvents(ctx, postgres.EventQuery{Tenant: tn})
	require.NoError(t, err)
	assert.Len(t, evs, 1, "CI retries must not duplicate events")
}

func TestAppendEventRejectsInvalid(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	bad := mkEvent(tn, "01A", "k1", "dep-7", "deployment.started", time.Now())
	bad.Subject = ""

	_, _, err := s.AppendEvent(ctx, bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subject")
}

// TestConcurrentAppendsToSameRefDoNotLoseUpdates is the reason the projection upsert
// takes a row lock. Without SELECT ... FOR UPDATE, two ingress replicas folding two
// events onto the same ref both read the same prior change and the second write
// silently discards the first event's contribution — no error, nothing in the logs.
func TestConcurrentAppendsToSameRefDoNotLoseUpdates(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

	_, _, err := s.AppendEvent(ctx, mkEvent(tn, "01C", "k0", "dep-7", "deployment.started", at))
	require.NoError(t, err)

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev := mkEvent(tn, fmt.Sprintf("02%02d", i), fmt.Sprintf("k%d", i+1),
				"dep-7", "deployment.annotated", at.Add(time.Duration(i)*time.Second))
			ev.Attributes = map[string]any{fmt.Sprintf("attr%d", i): "set"}
			_, _, err := s.AppendEvent(ctx, ev)
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()

	var attrs map[string]any
	err = s.Pool().QueryRow(ctx,
		`SELECT attributes FROM change WHERE tenant = $1 AND ref = $2`, tn, "dep-7").Scan(&attrs)
	require.NoError(t, err)

	for i := 0; i < n; i++ {
		assert.Contains(t, attrs, fmt.Sprintf("attr%d", i),
			"every concurrent event's contribution must survive; a missing key is a lost update")
	}
}
