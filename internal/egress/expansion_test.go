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

// expansionShapes are the data the factor is measured on: what producers are documented to send;
// H-3's shape of many entries of about 60 bytes; and short keys with small numbers, which cost
// the most heap per byte of text, since each number becomes a boxed float64 in a map entry. Row
// counts give each shape megabytes of row text, so background allocation is noise.
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

// realisticAttributes are the README's recommended keys, fully populated, with a 40-line release
// note as the description: about 3 KiB of JSON.
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

// seedShape inserts rows closed changes and rows events for tn, each carrying attrs, in one
// batched round trip that bypasses the fold, as seedManyChanges does.
func seedShape(t *testing.T, s *postgres.Store, tn string, rows int, attrs map[string]any) {
	t.Helper()
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	batch := &pgx.Batch{}
	for i := range rows {
		ref := fmt.Sprintf("shape-%d", i)
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
			                   correlation_ref, attributes)
			VALUES ($1,$2,$1,1,'ci','deployment.completed','acme/prod','ci-bot','',
			        $3,$3,$4,$5)`,
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

// expansionReads are the four renderings of tn's rows, each with the estimate admission makes
// for it. The POST annotations routes render exactly as the GET does.
func expansionReads(t *testing.T, s *postgres.Store, tn string) []expansionRead {
	t.Helper()
	ctx := context.Background()
	from := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	changes, err := s.EstimateChanges(ctx, postgres.ChangeQuery{
		Tenant: tn, From: from, To: from.Add(2 * time.Hour), Limit: egress.MaxLimit})
	require.NoError(t, err)
	events, err := s.EstimateEvents(ctx, postgres.EventQuery{Tenant: tn, Limit: egress.MaxLimit})
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

// allocatedDuring returns the bytes the process allocated on the heap while fn ran.
//
// runtime.ReadMemStats flushes every processor's allocation cache before reading, so its total
// is exact; the runtime otherwise records small allocations only when a cached span is refilled
// or released. Two collections beforehand empty every sync.Pool, so a buffer another read left
// pooled cannot hide what this one needs. The count is process-wide, so anything else running
// adds to it, which can only make the test fail, never pass.
func allocatedDuring(fn func()) uint64 {
	runtime.GC()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestReadAllocationsStayWithinExpansionFactor holds readExpansionFactor true. Admission reserves
// estimate × factor bytes for a read, so a read that allocates more than that can use more memory
// than it reserved. Decoding, rendering, dependencies and the Go runtime all move the ratio, and
// this test runs on every change to any of them.
func TestReadAllocationsStayWithinExpansionFactor(t *testing.T) {
	// This test measures what a whole read allocates, so admission must never cut one of its
	// reads short: a pool sized for production would legitimately cut the heaviest shapes below,
	// which would fail on the no-truncation precondition before any ratio was even computed. A
	// reservation is only a count against a semaphore, not an allocation, so a 16 GiB pool costs
	// this test nothing.
	limits := egress.DefaultLimits()
	limits.ReadMemoryBytes = 1 << 34
	srv, s, base := newServerWith(t, 0, limits)
	h := srv.Routes()
	for _, shape := range expansionShapes() {
		tn := testdb.NewTenant(t, base+"-"+shape.name)
		seedShape(t, s, tn, shape.rows, shape.attrs)
		// A tenant-scoped reader, not a superuser: a global credential's tenant is checked
		// against the store on every request, an extra allocation this test's ratio must
		// not have to absorb. Minted and warmed outside every measured closure below.
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
	//nolint:gosec // G115: TotalBytes is a measured row-text size and is never negative, and
	// readExpansionFactor is a small positive constant, so the product cannot overflow int64
	// far enough to invert sign before this conversion to uint64.
	assert.LessOrEqual(t, allocated, uint64(egress.ReadExpansionFactor*read.estimate.TotalBytes),
		"the read allocated %.2f bytes per byte of row text; readExpansionFactor is %d",
		ratio, egress.ReadExpansionFactor)
}
