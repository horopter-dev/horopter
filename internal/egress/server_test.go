package egress_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/core"
	"github.com/yo61/horopter/internal/egress"
	"github.com/yo61/horopter/internal/store/postgres"
)

var fixed = time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)

// The tests share a database that nothing truncates, so every tenant and event id a
// test puts into it has to be one no earlier execution used. procToken separates one
// `go test` process from the next; seedSeq and tenantSeq separate every call within a
// process, which is what makes -count=N (and rerunning the mutations below by hand)
// do real work instead of colliding with a prior run.
var (
	procToken = strconv.FormatInt(time.Now().UnixNano(), 36)
	seedSeq   atomic.Uint64
	tenantSeq atomic.Uint64
)

func newServer(t *testing.T) (http.Handler, *postgres.Store, string) {
	t.Helper()
	v := os.Getenv("HOROPTER_TEST_DSN")
	if v == "" {
		t.Skip("set HOROPTER_TEST_DSN to run egress tests (docker compose up -d db)")
	}
	s, err := postgres.Open(context.Background(), v)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	tn := "eg-" + t.Name() + "-" + procToken + "-" + strconv.FormatUint(tenantSeq.Add(1), 36)
	return egress.New(s, func() time.Time { return fixed }).Routes(), s, tn
}

// seed writes directly through the store: egress has no write path of its own. id
// includes procToken as well as a counter because event.id is a GLOBAL primary key: a
// counter alone resets to zero on every new `go test` process, so two separate
// invocations against the same never-truncated database would both mint
// "seed-TestX-1" and collide on the second run. procToken (set once at package init
// from the wall clock) is what makes the id unique across process invocations, not
// just within one.
func seed(t *testing.T, s *postgres.Store, tn, ref, typ string, at time.Time) {
	t.Helper()
	id := fmt.Sprintf("seed-%s-%s-%d", t.Name(), procToken, seedSeq.Add(1))
	_, _, err := s.AppendEvent(context.Background(), core.Event{
		ID: id, Tenant: tn, IdempotencyKey: id, SchemaVersion: 1,
		Source: "ci", Type: typ, Subject: "acme/prod", Actor: "ci-bot",
		OccurredAt: at, ReceivedAt: at, CorrelationRef: ref,
		Attributes: map[string]any{"service.version": "2.5.0"},
	})
	require.NoError(t, err)
}

func get(t *testing.T, h http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

func TestQueryChangesReturnsNeutralJSON(t *testing.T) {
	h, s, tn := newServer(t)
	seed(t, s, tn, "dep-7", "deployment.started",
		time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC))

	rec := get(t, h, "/v1/changes?tenant="+tn+
		"&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var changes []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &changes))
	require.Len(t, changes, 1)
	assert.Equal(t, "dep-7", changes[0]["ref"])
	assert.Equal(t, "in_progress", changes[0]["status"])
	assert.NotContains(t, changes[0], "timeEnd",
		"the neutral API must not leak a vendor field name")
}

func TestGrafanaAdapterUsesEpochMillis(t *testing.T) {
	h, s, tn := newServer(t)
	seed(t, s, tn, "dep-8", "deployment.started",
		time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC))

	rec := get(t, h, "/adapters/grafana/annotations?tenant="+tn+
		"&from=1789376400000&to=1789383600000")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var anns []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anns))
	require.Len(t, anns, 1)
	assert.EqualValues(t, 1789380000000, anns[0]["time"])
	assert.EqualValues(t, fixed.UnixMilli(), anns[0]["timeEnd"],
		"an open change ends at the injected clock, not the wall clock")
}

func TestOTLPAdapterUsesNanoseconds(t *testing.T) {
	h, s, tn := newServer(t)
	start := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	seed(t, s, tn, "dep-9", "deployment.started", start)

	rec := get(t, h, "/adapters/otlp/spans?tenant="+tn+
		"&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var spans []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &spans))
	require.Len(t, spans, 1)
	assert.EqualValues(t, start.UnixNano(), spans[0]["StartUnixNano"],
		"OTLP uses nanoseconds where Grafana uses milliseconds — the unit lives in the adapter")
}

func TestEgressRejectsMalformedTimeParams(t *testing.T) {
	h, _, tn := newServer(t)
	rec := get(t, h, "/v1/changes?tenant="+tn+"&from=not-a-time&to=2026-09-14T11:00:00Z")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "from")
}

func TestEgressHasNoWriteRoutes(t *testing.T) {
	h, _, _ := newServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code,
		"egress must expose no write path at all")
}

func TestListEventsReturnsRawLog(t *testing.T) {
	h, s, tn := newServer(t)
	seed(t, s, tn, "dep-10", "deployment.started",
		time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC))

	rec := get(t, h, "/v1/events?tenant="+tn+"&ref=dep-10")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var events []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &events))
	require.Len(t, events, 1)
	assert.Equal(t, "dep-10", events[0]["correlation_ref"])
	assert.Equal(t, "deployment.started", events[0]["type"])
}

func TestReadyzReportsReadyWithLiveDatabase(t *testing.T) {
	h, _, _ := newServer(t)
	rec := get(t, h, "/readyz")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestHealthzWorksWithNilStore proves healthz never touches the database, without
// needing a live one: New never dereferences its store argument, and a Server built
// with a nil store still has to answer 200 from healthz for that to hold. It runs
// unconditionally — no HOROPTER_TEST_DSN skip, and driven through the real Routes()
// mux rather than calling the handler directly — because it is the one property in
// this suite that a database-less CI leg can actually exercise: every other test here
// skips without a live Postgres, so without this one the liveness/readiness split
// would have zero coverage in that environment. If a future change reintroduces a
// Pool().Ping call into healthz, this test panics on the nil store and fails.
func TestHealthzWorksWithNilStore(t *testing.T) {
	srv := egress.New(nil, func() time.Time { return fixed })
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "healthz must not require a database")
}
