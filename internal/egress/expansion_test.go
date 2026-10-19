//go:build !race

package egress_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/egress"
	"horopter.dev/horopter/internal/store/postgres"
	"horopter.dev/horopter/internal/testdb"
)

type expansionShape struct {
	name  string
	rows  int
	attrs map[string]any
}

// Short keys with small numbers cost the most heap per byte: each number is a boxed float64.
func expansionShapes() []expansionShape {
	manyKeys := make(map[string]any, 500)
	for i := range 500 {
		manyKeys[fmt.Sprintf("build.step.%04d.log", i)] = strings.Repeat("v", 40)
	}
	numbers := make(map[string]any, 2000)
	for i := range 2000 {
		numbers[fmt.Sprintf("n%d", i)] = i
	}
	return []expansionShape{
		{"realistic", 1000, realisticAttributes()},
		{"many-60-byte-entries", 200, manyKeys},
		{"short-numeric-keys", 250, numbers},
	}
}

func realisticAttributes() map[string]any {
	return map[string]any{
		"service.name":                  "checkout-api",
		"service.version":               "2.5.0",
		"deployment.environment.name":   "prod",
		"cicd.pipeline.name":            "deploy-production",
		"cicd.pipeline.result":          "success",
		"cicd.pipeline.task.name":       "rollout",
		"cicd.pipeline.task.run.id":     "4817",
		"cicd.pipeline.task.run.result": "success",
		"cloudevents.event_id":          "01J8Z3Q4R5S6T7V8W9X0Y1Z2A3",
		"cloudevents.event_source":      "buildkite/pipelines/checkout-api",
		"cloudevents.event_type":        "deployment.completed",
		"cloudevents.event_subject":     "acme/prod",
		"horopter.description": strings.Repeat(
			"- Retry the registry push when the upstream mirror times out\n", 40),
	}
}

func seedShape(t *testing.T, s *postgres.Store, tn string, rows int, attrs map[string]any) {
	t.Helper()
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	batch := &pgx.Batch{}
	for i := range rows {
		ref := fmt.Sprintf("ci/shape-%d", i)
		batch.Queue(`
			INSERT INTO change (tenant, ref, subject, type, title, actor, started_at,
			                    first_seen_at, end_at, status, attributes, last_event_id)
			VALUES ($1,$2,'acme/prod','deployment.completed','Deploy checkout-api','ci-bot',
			        $3,$3,$3,'completed',$4,'seed')`,
			tn, ref, at, attrs)
		id := tn + "-" + ref
		batch.Queue(`
			INSERT INTO event (id, tenant, idempotency_key, schema_version, source, type,
			                   subject, actor, title, occurred_at, received_at,
			                   correlation_ref, attributes, locus)
			VALUES ($1,$2,$1,1,'ci','deployment.completed','acme/prod','ci-bot','',
			        $3,$3,$4,$5,'ci')`,
			id, tn, at, ref, attrs)
	}
	br := s.Pool().SendBatch(context.Background(), batch)
	defer func() { require.NoError(t, br.Close()) }()
	for range 2 * rows {
		_, err := br.Exec()
		require.NoError(t, err)
	}
}

type expansionRead struct {
	route, url string
	estimate   postgres.ReadEstimate
}

func expansionReads(t *testing.T, s *postgres.Store, tn string) []expansionRead {
	t.Helper()
	ctx := context.Background()
	from := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	changes, err := s.EstimateChanges(ctx,
		changeQuery(t, tn, from, from.Add(2*time.Hour), egress.MaxLimit, nil))
	require.NoError(t, err)
	events, err := s.EstimateEvents(ctx, eventQuery(t, tn, egress.MaxLimit))
	require.NoError(t, err)
	require.GreaterOrEqual(t, min(changes.TotalBytes, events.TotalBytes), int64(2<<20),
		"each shape needs megabytes of row text, or background allocation stops being noise")

	window := "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	return []expansionRead{
		{"changes", "/v1/changes?tenant=" + tn + window, changes},
		{"events", "/v1/events?tenant=" + tn, events},
		{"grafana", "/adapters/grafana/annotations?tenant=" + tn +
			"&from=1789376400000&to=1789383600000", changes},
		{"otlp", "/adapters/otlp/spans?tenant=" + tn + window, changes},
	}
}

// ReadMemStats flushes per-P caches, so the total is exact; the GCs empty every sync.Pool.
func allocatedDuring(fn func()) uint64 {
	runtime.GC()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestReadAllocationsStayWithinExpansionFactor(t *testing.T) {
	// A huge pool so admission never truncates: a reservation allocates nothing.
	limits := egress.DefaultLimits()
	limits.ReadMemoryBytes = 1 << 34
	srv, s, base := newServerWith(t, 0, limits)
	h := srv.Routes()
	for _, shape := range expansionShapes() {
		tn := testdb.NewTenant(t, base+"-"+shape.name)
		seedShape(t, s, tn, shape.rows, shape.attrs)
		// Tenant-scoped, not superuser: a global credential's tenant check allocates per request.
		key := readerKey(t, s, tn)
		warmCredentialCache(t, h, key, tn)
		for _, read := range expansionReads(t, s, tn) {
			t.Run(shape.name+"-"+read.route, func(t *testing.T) {
				assertWithinExpansionFactor(t, h, key, read)
			})
		}
	}
}

func assertWithinExpansionFactor(t *testing.T, h http.Handler, key string, read expansionRead) {
	t.Helper()
	var rec *httptest.ResponseRecorder
	allocated := allocatedDuring(func() { rec = get(t, h, key, read.url) })

	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Header().Get(egress.TruncatedHeader),
		"the read must be whole, or it allocated for less than it was estimated at")
	ratio := float64(allocated) / float64(read.estimate.TotalBytes)
	t.Logf("%d bytes allocated for %d bytes of row text: ratio %.2f",
		allocated, read.estimate.TotalBytes, ratio)
	//nolint:gosec // G115: both factors are small and non-negative, so the product cannot overflow.
	assert.LessOrEqual(t, allocated, uint64(egress.ReadExpansionFactor*read.estimate.TotalBytes),
		"the read allocated %.2f bytes per byte of row text; readExpansionFactor is %d",
		ratio, egress.ReadExpansionFactor)
}
