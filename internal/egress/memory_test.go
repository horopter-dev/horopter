package egress_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/egress"
	"github.com/yo61/horopter/internal/store/postgres"
)

// memoryFixture is three changes in changesURL's window and two estimates of them: firstTwo, from
// a limit of 1, which over-fetches the second row, and all, as a read of changesURL makes.
type memoryFixture struct {
	s        *postgres.Store
	tn       string
	firstTwo postgres.ReadEstimate
	all      postgres.ReadEstimate
}

func newMemoryFixture(t *testing.T) memoryFixture {
	t.Helper()
	s, tn := openTestStore(t, 0)
	base := time.Date(2026, 9, 14, 9, 30, 0, 0, time.UTC)
	for i, ref := range []string{"mem-a", "mem-b", "mem-c"} {
		seed(t, s, tn, ref, "deployment.started", base.Add(time.Duration(i)*10*time.Minute))
	}
	q := postgres.ChangeQuery{
		Tenant: tn,
		From:   time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC),
		To:     time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC),
		Limit:  1,
	}
	firstTwo, err := s.EstimateChanges(context.Background(), q)
	require.NoError(t, err)
	q.Limit = egress.MaxLimit
	all, err := s.EstimateChanges(context.Background(), q)
	require.NoError(t, err)
	require.Equal(t, 3, all.Rows)
	return memoryFixture{s: s, tn: tn, firstTwo: firstTwo, all: all}
}

// server serves the fixture with a read memory pool of capacity bytes.
func (f memoryFixture) server(capacity int64) *egress.Server {
	limits := testLimits()
	limits.ReadMemoryBytes = capacity
	return egress.New(f.s, fixedClock, limits)
}

func bytesAttr(n int64) []string { return []string{strconv.FormatInt(n, 10)} }

func TestAReadThatFitsTheMemoryPoolIsServedWhole(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()
	f := newMemoryFixture(t)
	srv := f.server(egress.ReadExpansionFactor * f.all.TotalBytes)

	resp := get(t, srv.Routes(), changesURL(f.tn))

	assert.Equal(t, []string{"mem-a", "mem-b", "mem-c"}, refsIn(t, resp))
	assert.Empty(t, resp.Header().Get(egress.TruncatedHeader))
	assert.Equal(t, bytesAttr(f.all.TotalBytes),
		spanAttrValues(rec, "", "horopter.read.estimated_bytes"))
	assert.Equal(t, bytesAttr(egress.ReadExpansionFactor*f.all.TotalBytes),
		spanAttrValues(rec, "", "horopter.read.reserved_bytes"))
	assert.Equal(t, []string{"false"}, spanAttrValues(rec, "", "horopter.read.byte_cut"))
	assert.Equal(t, int64(0), egress.ReadMemoryReserved(srv),
		"a finished read gives its reservation back")
}

// TestAReadLargerThanTheMemoryPoolIsCutAndMarked sizes the pool to hold exactly the first two
// rows, so the whole read cannot fit but its first row can.
func TestAReadLargerThanTheMemoryPoolIsCutAndMarked(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()
	f := newMemoryFixture(t)
	capacity := egress.ReadExpansionFactor * f.firstTwo.TotalBytes
	srv := f.server(capacity)

	resp := get(t, srv.Routes(), changesURL(f.tn))

	assert.Equal(t, []string{"mem-a", "mem-b"}, refsIn(t, resp),
		"the oldest rows that fit the pool")
	assert.Equal(t, "true", resp.Header().Get(egress.TruncatedHeader))
	assert.Equal(t, []string{"true"}, spanAttrValues(rec, "", "horopter.read.byte_cut"))
	assert.Equal(t, bytesAttr(capacity), spanAttrValues(rec, "", "horopter.read.reserved_bytes"))
	assert.Equal(t, []string{"admitted"}, spanAttrValues(rec, "", "horopter.admission.outcome"))
}

func TestAReadWhoseFirstRowExceedsTheMemoryPoolIsRefused(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()
	f := newMemoryFixture(t)
	capacity := egress.ReadExpansionFactor*f.firstTwo.FirstRowBytes - 1
	srv := f.server(capacity)
	records := captureLogs(t)

	resp := get(t, srv.Routes(), changesURL(f.tn))

	require.Equal(t, http.StatusUnprocessableEntity, resp.Code, resp.Body.String())
	assert.Contains(t, resp.Body.String(),
		fmt.Sprintf("larger than egress's read memory limit of %d bytes allows", capacity))
	assert.NotContains(t, resp.Body.String(), f.tn)
	warnings := recordsWithMessage(records(), "read too large")
	require.Len(t, warnings, 1, "got %v", records())
	w := warnings[0]
	assert.Equal(t, "WARN", w["level"])
	assert.Equal(t, "GET /v1/changes", w["route"])
	assert.Equal(t, f.tn, w["tenant"])
	assert.Equal(t, float64(f.firstTwo.FirstRowBytes), w["first_row_bytes"])
	assert.Equal(t, float64(egress.ReadExpansionFactor*f.firstTwo.FirstRowBytes), w["needed_bytes"])
	assert.Equal(t, float64(capacity), w["capacity_bytes"])
	assert.Equal(t, []string{"too_large"}, spanAttrValues(rec, "", "horopter.admission.outcome"))
}

func TestAReadIsShedWhileTheMemoryPoolIsHeld(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()
	f := newMemoryFixture(t)
	capacity := egress.ReadExpansionFactor * f.all.TotalBytes
	srv := f.server(capacity)
	h := srv.Routes()
	release, err := egress.ReserveReadMemory(srv, capacity)
	require.NoError(t, err)
	records := captureLogs(t)

	shed := get(t, h, changesURL(f.tn))
	release()
	served := get(t, h, changesURL(f.tn))

	require.Equal(t, http.StatusServiceUnavailable, shed.Code, shed.Body.String())
	assert.Equal(t, "1", shed.Header().Get("Retry-After"))
	require.Equal(t, http.StatusOK, served.Code, "the read fits once the pool is free")
	warnings := recordsWithMessage(records(), "read shed")
	require.Len(t, warnings, 1, "got %v", records())
	w := warnings[0]
	assert.Equal(t, "memory", w["gate"])
	assert.Equal(t, float64(capacity), w["needed_bytes"])
	assert.Equal(t, float64(capacity), w["reserved_bytes"])
	assert.Equal(t, float64(capacity), w["capacity_bytes"])
	assert.ElementsMatch(t, []string{"shed_memory", "admitted"},
		spanAttrValues(rec, "", "horopter.admission.outcome"))
}
