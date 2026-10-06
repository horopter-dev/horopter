package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/store/postgres"
)

var sizedBase = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

// sizedChanges seeds three changes ten minutes apart whose single attribute's value is 1, 11
// and 21 bytes long, and returns their refs in start order with each row's measured size.
func sizedChanges(t *testing.T, s *postgres.Store, tn string) ([]string, []int64) {
	t.Helper()
	producerRefs := []string{"sized-a", "sized-b", "sized-c"}
	refs := make([]string, 0, len(producerRefs))
	sizes := make([]int64, 0, len(producerRefs))
	for i, producerRef := range producerRefs {
		ev := mkEvent(tn, eventID(t, producerRef), "k-"+producerRef, producerRef,
			"deployment.started", sizedBase.Add(time.Duration(i)*10*time.Minute))
		ev.Attributes = map[string]any{"k": strings.Repeat("x", 1+10*i)}
		_, _, err := s.AppendEvent(context.Background(), ev)
		require.NoError(t, err)
		// The stored ref, not the producer's: mkEvent composes the locus onto it, and a
		// row is only measurable by the ref it was actually written under.
		ref := core.RefFor(ev).String()
		refs = append(refs, ref)
		sizes = append(sizes, changeRowBytes(t, s, tn, ref))
	}
	return refs, sizes
}

// changeRowBytes measures one change row with the simplest query that can: the oracle the
// estimate and byte-cut tests compare against, independent of the aggregate over a limited,
// filtered subquery that the code under test runs.
func changeRowBytes(t *testing.T, s *postgres.Store, tn, ref string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, s.Pool().QueryRow(context.Background(),
		`SELECT octet_length(c::text) FROM change c WHERE c.tenant = $1 AND c.ref = $2`,
		tn, ref).Scan(&n))
	return n
}

// eventRowBytes measures every event row tn holds, lowest id first, as changeRowBytes does.
func eventRowBytes(t *testing.T, s *postgres.Store, tn string) []int64 {
	t.Helper()
	rows, err := s.Pool().Query(context.Background(),
		`SELECT octet_length(e::text) FROM event e WHERE e.tenant = $1 ORDER BY e.id`, tn)
	require.NoError(t, err)
	sizes, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	require.NoError(t, err)
	return sizes
}

func sizedWindow(t *testing.T, tn string, limit int) postgres.ChangeQuery {
	t.Helper()
	return changeQuery(t, tn, sizedBase.Add(-time.Hour), sizedBase.Add(time.Hour), limit, nil)
}

func changeRefs(changes []core.Change) []string {
	var refs []string
	for _, c := range changes {
		refs = append(refs, c.Ref.String())
	}
	return refs
}

func TestEstimateChangesMeasuresTheRowsQueryChangesReads(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	_, sizes := sizedChanges(t, s, tn)
	require.Equal(t, int64(10), sizes[1]-sizes[0],
		"a value 10 bytes longer makes a row 10 bytes longer; if not, the oracle is wrong")

	est, err := s.EstimateChanges(ctx, sizedWindow(t, tn, 10))

	require.NoError(t, err)
	assert.Equal(t, postgres.ReadEstimate{
		Rows: 3, TotalBytes: sizes[0] + sizes[1] + sizes[2], FirstRowBytes: sizes[0],
	}, est)
}

func TestEstimateChangesAppliesTheQuerysFiltersAndLimit(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	_, sizes := sizedChanges(t, s, tn)
	other := mkEvent(tn, eventID(t, "other"), "k-other", "sized-other", "deployment.started",
		sizedBase.Add(5*time.Minute))
	other.Subject = "other/env"
	_, _, err := s.AppendEvent(ctx, other)
	require.NoError(t, err)

	filtered := sizedWindow(t, tn, 10)
	filtered.Subjects = mustSubjects(t, "acme/prod")
	est, err := s.EstimateChanges(ctx, filtered)
	require.NoError(t, err)
	assert.Equal(t, postgres.ReadEstimate{
		Rows: 3, TotalBytes: sizes[0] + sizes[1] + sizes[2], FirstRowBytes: sizes[0],
	}, est, "the subject filter must exclude the other change")

	limited := sizedWindow(t, tn, 1)
	limited.Subjects = mustSubjects(t, "acme/prod")
	est, err = s.EstimateChanges(ctx, limited)
	require.NoError(t, err)
	got, truncated, err := s.QueryChanges(ctx, limited)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.True(t, truncated)
	assert.Equal(t, postgres.ReadEstimate{
		Rows: 2, TotalBytes: sizes[0] + sizes[1], FirstRowBytes: sizes[0],
	}, est, "a limit of 1 fetches an over-fetch row, and the estimate must count it")
}

func TestEstimateOfAnEmptyReadIsZero(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)

	changes, err := s.EstimateChanges(ctx, sizedWindow(t, tn, 10))
	require.NoError(t, err)
	events, err := s.EstimateEvents(ctx, eventQuery(t, tn, 10))
	require.NoError(t, err)

	assert.Equal(t, postgres.ReadEstimate{}, changes)
	assert.Equal(t, postgres.ReadEstimate{}, events)
}

func TestEstimateEventsMeasuresTheRowsListEventsReads(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	refs, _ := sizedChanges(t, s, tn)
	sizes := eventRowBytes(t, s, tn)
	require.Len(t, sizes, 3)

	all, err := s.EstimateEvents(ctx, eventQuery(t, tn, 10))
	require.NoError(t, err)
	one, err := s.EstimateEvents(ctx, refQuery(t, tn, refs[1]))
	require.NoError(t, err)

	assert.Equal(t, postgres.ReadEstimate{
		Rows: 3, TotalBytes: sizes[0] + sizes[1] + sizes[2], FirstRowBytes: sizes[0],
	}, all)
	assert.Equal(t, postgres.ReadEstimate{Rows: 1, TotalBytes: sizes[1], FirstRowBytes: sizes[1]},
		one, "the ref filter must apply")
}

func TestQueryChangesCutsAtTheByteBudget(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	refs, sizes := sizedChanges(t, s, tn)
	every := sizes[0] + sizes[1] + sizes[2]

	for _, tc := range []struct {
		name      string
		budget    int64
		limit     int
		want      []string
		truncated bool
	}{
		{"after the second row", sizes[0] + sizes[1], 10, refs[:2], true},
		{"one byte short of every row", every - 1, 10, refs[:2], true},
		{"exactly every row", every, 10, refs, false},
		{"smaller than the first row", sizes[0] - 1, 10, nil, true},
		{"within budget but over the limit", every, 2, refs[:2], true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := sizedWindow(t, tn, tc.limit)
			q.MaxRowTextBytes = tc.budget

			got, truncated, err := s.QueryChanges(ctx, q)

			require.NoError(t, err)
			assert.Equal(t, tc.want, changeRefs(got), "the oldest rows that fit, in order")
			assert.Equal(t, tc.truncated, truncated)
		})
	}
}

func TestListEventsCutsAtTheByteBudget(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	sizedChanges(t, s, tn)
	sizes := eventRowBytes(t, s, tn)
	require.Len(t, sizes, 3)

	for _, tc := range []struct {
		name      string
		budget    int64
		rows      int
		truncated bool
	}{
		{"after the first row", sizes[0], 1, true},
		{"exactly every row", sizes[0] + sizes[1] + sizes[2], 3, false},
		{"smaller than the first row", sizes[0] - 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated, err := s.ListEvents(ctx,
				cutEventQuery(t, tn, 10, tc.budget))

			require.NoError(t, err)
			assert.Len(t, got, tc.rows)
			assert.Equal(t, tc.truncated, truncated)
		})
	}
}

// TestAByteCutOfAnEmptyReadIsNotTruncated guards the other side of the zero-row case: a cut
// that returns nothing because there was nothing must not claim rows were discarded.
func TestAByteCutOfAnEmptyReadIsNotTruncated(t *testing.T) {
	ctx, s, tn := context.Background(), newStore(t), tenant(t)
	q := sizedWindow(t, tn, 10)
	q.MaxRowTextBytes = 1

	got, truncated, err := s.QueryChanges(ctx, q)

	require.NoError(t, err)
	assert.Empty(t, got)
	assert.False(t, truncated)
}
