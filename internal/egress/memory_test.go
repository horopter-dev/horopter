package egress_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/egress"
	"horopter.dev/horopter/internal/store/postgres"
)

// memoryFixture is three changes in changesURL's window, each seeded by one event, and estimates
// of both: firstTwo, from a limit of 1, which over-fetches the second row, and all, as a read of
// changesURL/eventsURL makes. key is a reader credential scoped to tn.
type memoryFixture struct {
	s              *postgres.Store
	tn             string
	key            string
	firstTwo       postgres.ReadEstimate
	all            postgres.ReadEstimate
	eventsFirstTwo postgres.ReadEstimate
	eventsAll      postgres.ReadEstimate
}

func newMemoryFixture(t *testing.T) memoryFixture {
	t.Helper()
	s, tn := openTestStore(t, 0)
	base := time.Date(2026, 9, 14, 9, 30, 0, 0, time.UTC)
	for i, ref := range []string{"ci/mem-a", "ci/mem-b", "ci/mem-c"} {
		seed(t, s, tn, ref, "deployment.started", base.Add(time.Duration(i)*10*time.Minute))
	}
	q := changeQuery(t, tn,
		time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC), 1, nil)
	firstTwo, err := s.EstimateChanges(context.Background(), q)
	require.NoError(t, err)
	q.Limit = egress.MaxLimit
	all, err := s.EstimateChanges(context.Background(), q)
	require.NoError(t, err)
	require.Equal(t, 3, all.Rows)

	eq := eventQuery(t, tn, 1)
	eventsFirstTwo, err := s.EstimateEvents(context.Background(), eq)
	require.NoError(t, err)
	eq.Limit = egress.MaxLimit
	eventsAll, err := s.EstimateEvents(context.Background(), eq)
	require.NoError(t, err)
	require.Equal(t, 3, eventsAll.Rows)

	return memoryFixture{
		s: s, tn: tn, key: readerKey(t, s, tn), firstTwo: firstTwo, all: all,
		eventsFirstTwo: eventsFirstTwo, eventsAll: eventsAll,
	}
}

// server serves the fixture with a read memory pool of capacity bytes.
func (f memoryFixture) server(t *testing.T, capacity int64) *egress.Server {
	t.Helper()
	limits := testLimits()
	limits.ReadMemoryBytes = capacity
	authn := auth.New(auth.NewStore(f.s.Pool()), fixedClock, auth.DefaultOptions())
	srv, err := egress.New(f.s, fixedClock, limits, authn)
	require.NoError(t, err)
	return srv
}

func bytesAttr(n int64) []string { return []string{strconv.FormatInt(n, 10)} }

func eventsURL(tn string) string { return "/v1/events?tenant=" + tn }

// tooLargeBody is the exact 422 body a read whose first row alone exceeds capacity gets.
func tooLargeBody(capacity int64) string {
	return fmt.Sprintf(
		`{"error":"the first row of this read is larger than egress's read memory limit of `+
			`%d bytes allows; narrow the read to a subject, type or status that row does not `+
			`have (on /v1/events, to a ref), or raise HOROPTER_EGRESS_READ_MEMORY_BYTES"}`,
		capacity)
}

const shedBody = `{"error":"egress is at capacity; retry after the interval in Retry-After"}`

func TestAReadThatFitsTheMemoryPoolIsServedWhole(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()
	f := newMemoryFixture(t)
	srv := f.server(t, egress.ReadExpansionFactor*f.all.TotalBytes)

	resp := get(t, srv.Routes(), f.key, changesURL(f.tn))

	assert.Equal(t, []string{"ci/mem-a", "ci/mem-b", "ci/mem-c"}, refsIn(t, resp))
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
	srv := f.server(t, capacity)

	resp := get(t, srv.Routes(), f.key, changesURL(f.tn))

	assert.Equal(t, []string{"ci/mem-a", "ci/mem-b"}, refsIn(t, resp),
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
	srv := f.server(t, capacity)
	records := captureLogs(t)

	resp := get(t, srv.Routes(), f.key, changesURL(f.tn))

	require.Equal(t, http.StatusUnprocessableEntity, resp.Code, resp.Body.String())
	assert.JSONEq(t, tooLargeBody(capacity), resp.Body.String())
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
	srv := f.server(t, capacity)
	h := srv.Routes()
	release, err := egress.ReserveReadMemory(srv, capacity)
	require.NoError(t, err)
	records := captureLogs(t)

	shed := get(t, h, f.key, changesURL(f.tn))
	release()
	served := get(t, h, f.key, changesURL(f.tn))

	require.Equal(t, http.StatusServiceUnavailable, shed.Code, shed.Body.String())
	assert.JSONEq(t, shedBody, shed.Body.String())
	assert.Equal(t, "1", shed.Header().Get("Retry-After"))
	require.Equal(t, http.StatusOK, served.Code, "the read fits once the pool is free")
	warnings := recordsWithMessage(records(), "read shed")
	require.Len(t, warnings, 1, "got %v", records())
	w := warnings[0]
	assert.Equal(t, "memory", w["gate"])
	assert.Equal(t, float64(capacity), w["needed_bytes"])
	assert.Equal(t, float64(capacity), w["reserved_bytes"])
	assert.Equal(t, float64(capacity), w["capacity_bytes"])
	assert.GreaterOrEqual(t, w["waited_ms"], float64(40),
		"the read waited out the admission wait before being shed")
	assert.ElementsMatch(t, []string{"shed_memory", "admitted"},
		spanAttrValues(rec, "", "horopter.admission.outcome"))
}

// TestAnEventsReadLargerThanTheMemoryPoolIsCutAndMarked is
// TestAReadLargerThanTheMemoryPoolIsCutAndMarked's counterpart for /v1/events: a separate route,
// a separate EstimateEvents/ListEvents pair, and its own independent admitMemory call.
func TestAnEventsReadLargerThanTheMemoryPoolIsCutAndMarked(t *testing.T) {
	f := newMemoryFixture(t)
	capacity := egress.ReadExpansionFactor * f.eventsFirstTwo.TotalBytes
	srv := f.server(t, capacity)

	resp := get(t, srv.Routes(), f.key, eventsURL(f.tn))

	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	events := eventsBody(t, resp)
	assert.Len(t, events, 2, "the oldest events that fit the pool")
	assert.Equal(t, "true", resp.Header().Get(egress.TruncatedHeader))
}

// TestAnEventsReadWhoseFirstRowExceedsTheMemoryPoolIsRefused is
// TestAReadWhoseFirstRowExceedsTheMemoryPoolIsRefused's counterpart for /v1/events.
func TestAnEventsReadWhoseFirstRowExceedsTheMemoryPoolIsRefused(t *testing.T) {
	f := newMemoryFixture(t)
	capacity := egress.ReadExpansionFactor*f.eventsFirstTwo.FirstRowBytes - 1
	srv := f.server(t, capacity)

	resp := get(t, srv.Routes(), f.key, eventsURL(f.tn))

	require.Equal(t, http.StatusUnprocessableEntity, resp.Code, resp.Body.String())
	assert.JSONEq(t, tooLargeBody(capacity), resp.Body.String())
}

// TestAMemoryCutPageOffersNextThatResumes: a page the pool cuts short is a page, not a dead
// end. Following next under the same small pool reaches every change the fixture holds.
func TestAMemoryCutPageOffersNextThatResumes(t *testing.T) {
	f := newMemoryFixture(t)
	small := f.server(t, egress.ReadExpansionFactor*f.firstTwo.TotalBytes)
	first := get(t, small.Routes(), f.key, changesURL(f.tn))
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.Equal(t, []string{"ci/mem-a", "ci/mem-b"}, refsIn(t, first), "cut to what fits")
	assert.NotEmpty(t, first.Header().Get("Link"), "a cut page offers the next one")

	rows, _ := follow(t, small.Routes(), f.key, changesURL(f.tn), "changes")
	whole := changesBody(t, get(t, f.server(t, 1<<30).Routes(), f.key, changesURL(f.tn)))
	assert.Len(t, rows, len(whole), "paging under the small pool reaches every change once")
}

// TestARefusedReadOffersNoNext: a first row larger than the pool is refused with 422, as read
// admission already does, and never answered 200 with a next it could not continue from.
func TestARefusedReadOffersNoNext(t *testing.T) {
	f := newMemoryFixture(t)
	srv := f.server(t, egress.ReadExpansionFactor*f.firstTwo.FirstRowBytes-1)
	resp := get(t, srv.Routes(), f.key, changesURL(f.tn))
	require.Equal(t, http.StatusUnprocessableEntity, resp.Code, resp.Body.String())
	assert.Empty(t, resp.Header().Get("Link"))
	assert.NotContains(t, resp.Body.String(), `"next"`)
}

// TestALaterPageIsAdmittedOnItsOwnFirstRow: each page is estimated with its own cursor, so a
// page whose first row is too large for the pool is refused like any first page would be. An
// estimate taken without the cursor sizes page two by page one's first row and admits it.
func TestALaterPageIsAdmittedOnItsOwnFirstRow(t *testing.T) {
	s, tn := openTestStore(t, 0)
	base := time.Date(2026, 9, 14, 9, 30, 0, 0, time.UTC)
	seed(t, s, tn, "ci/mem-small", "deployment.started", base)
	id := core.NewULID(time.Now(), rand.Reader)
	_, _, err := s.AppendEvent(context.Background(), core.Event{
		ID: id, Tenant: tn, IdempotencyKey: id, SchemaVersion: core.CurrentSchemaVersion,
		Source: "ci", Type: "deployment.started", Subject: "acme/prod",
		Title:      strings.Repeat("x", 4000),
		OccurredAt: base.Add(time.Minute), ReceivedAt: base.Add(time.Minute),
		CorrelationRef: core.RefFromRow("ci/mem-large"),
		Origin:         core.OriginProcess, Locus: core.LocusFromRow("ci"),
	})
	require.NoError(t, err)
	small, err := s.EstimateChanges(context.Background(), changeQuery(t, tn,
		time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC), 1, nil))
	require.NoError(t, err)
	f := memoryFixture{s: s, tn: tn, key: readerKey(t, s, tn)}
	srv := f.server(t, egress.ReadExpansionFactor*small.FirstRowBytes*3)

	first := get(t, srv.Routes(), f.key, changesURL(tn)+"&limit=1")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var body struct {
		Next string `json:"next"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &body))
	require.NotEmpty(t, body.Next, "page one offers the next page")

	second := get(t, srv.Routes(), f.key, body.Next)
	assert.Equal(t, http.StatusUnprocessableEntity, second.Code,
		"page two's first row is larger than the pool: %s", second.Body.String())
}
