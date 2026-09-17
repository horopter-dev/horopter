package egress_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/core"
	"github.com/yo61/horopter/internal/egress"
	"github.com/yo61/horopter/internal/store/postgres"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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
	srv, s, tn := newServerWith(t, 0, egress.DefaultLimits())
	return srv.Routes(), s, tn
}

// newServerWith serves a store whose pool holds poolMaxConns connections — zero leaves pgxpool's
// default — under limits. The concurrency limit starts at the pool size, so a small pool is how a
// test lets one held read fill it.
func newServerWith(
	t *testing.T, poolMaxConns int, limits egress.Limits,
) (*egress.Server, *postgres.Store, string) {
	t.Helper()
	s, tn := openTestStore(t, poolMaxConns)
	return egress.New(s, fixedClock, limits), s, tn
}

// openTestStore opens the test database with a pool of poolMaxConns connections, zero for
// pgxpool's default, and returns it with a tenant no other test uses.
func openTestStore(t *testing.T, poolMaxConns int) (*postgres.Store, string) {
	t.Helper()
	v := os.Getenv("HOROPTER_TEST_DSN")
	if v == "" {
		t.Skip("set HOROPTER_TEST_DSN to run egress tests (docker compose up -d db)")
	}
	if poolMaxConns > 0 {
		u, err := url.Parse(v)
		require.NoError(t, err)
		q := u.Query()
		q.Set("pool_max_conns", strconv.Itoa(poolMaxConns))
		u.RawQuery = q.Encode()
		v = u.String()
	}
	s, err := postgres.Open(context.Background(), v)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	tn := "eg-" + t.Name() + "-" + procToken + "-" + strconv.FormatUint(tenantSeq.Add(1), 36)
	return s, tn
}

func fixedClock() time.Time { return fixed }

// seed writes directly through the store: egress has no write path of its own.
// core.Event.Validate requires event.id to be a syntactically valid ULID, so id comes
// from core.NewULID with crypto/rand entropy — the same source production uses — rather
// than a counter-built string; crypto/rand's 80 bits of entropy per millisecond is what
// keeps ids unique across every tenant and every execution of this never-truncated
// database, the same guarantee production relies on.
func seed(t *testing.T, s *postgres.Store, tn, ref, typ string, at time.Time) {
	t.Helper()
	id := core.NewULID(time.Now(), rand.Reader)
	_, _, err := s.AppendEvent(context.Background(), core.Event{
		ID: id, Tenant: tn, IdempotencyKey: id, SchemaVersion: 1,
		Source: "ci", Type: typ, Subject: "acme/prod", Actor: "ci-bot",
		OccurredAt: at, ReceivedAt: at, CorrelationRef: ref,
		Attributes: map[string]any{"service.version": "2.5.0"},
	})
	require.NoError(t, err)
}

// seedManyEvents inserts n raw event rows in a single batched round trip, bypassing
// AppendEvent's projection fold entirely. The MaxLimit truncation tests need more
// rows than the cap, and folding each one into a change through a full transaction
// (as AppendEvent does) would make that setup slow for no benefit: these tests only
// exercise the HTTP layer's limit handling, not the store's write path, which is
// already covered elsewhere and which egress does not call in the first place.
func seedManyEvents(t *testing.T, s *postgres.Store, tn string, n int) {
	t.Helper()
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	batch := &pgx.Batch{}
	for range n {
		id := fmt.Sprintf("bulk-%s-%s-%d", t.Name(), procToken, seedSeq.Add(1))
		batch.Queue(`
			INSERT INTO event (id, tenant, idempotency_key, schema_version, source, type,
			                   subject, actor, title, occurred_at, received_at,
			                   correlation_ref, attributes)
			VALUES ($1,$2,$3,1,'ci','deployment.started','acme/prod','ci-bot','',
			        $4,$4,'bulk-ref','{}'::jsonb)`,
			id, tn, id, at)
	}
	br := s.Pool().SendBatch(context.Background(), batch)
	defer func() { require.NoError(t, br.Close()) }()
	for range n {
		_, err := br.Exec()
		require.NoError(t, err)
	}
}

// seedManyChanges inserts n raw change rows directly, one per distinct ref, for the
// same reason and by the same means as seedManyEvents: QueryChanges reads only the
// change table, so this is the cheapest way to put more than MaxLimit rows in front
// of it.
func seedManyChanges(t *testing.T, s *postgres.Store, tn string, n int) {
	t.Helper()
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	batch := &pgx.Batch{}
	for range n {
		ref := fmt.Sprintf("bulk-%s-%s-%d", t.Name(), procToken, tenantSeq.Add(1))
		// start_at and end_effective are omitted deliberately: Postgres generates both,
		// and a generated column rejects any value an INSERT supplies for it. started_at
		// carries the asserted start these rows stand in for; first_seen_at repeats it,
		// which is what a ref whose earliest event IS its start event folds to.
		batch.Queue(`
			INSERT INTO change (tenant, ref, subject, type, title, actor, started_at,
			                     first_seen_at, end_at, status, attributes, last_event_id)
			VALUES ($1,$2,'acme/prod','deployment.started','','ci-bot',$3,$3,NULL,
			        'in_progress','{}'::jsonb,'seed')`,
			tn, ref, at)
	}
	br := s.Pool().SendBatch(context.Background(), batch)
	defer func() { require.NoError(t, br.Close()) }()
	for range n {
		_, err := br.Exec()
		require.NoError(t, err)
	}
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

// TestGrafanaRejectsMalformedEpochMillis exists because
// TestEgressRejectsMalformedTimeParams only exercises rfc3339Param, via /v1/changes —
// nothing drove a malformed value through epochMillisParam, the parser the Grafana
// endpoint actually uses. The needle is "epoch milliseconds" rather than "from":
// an earlier assertion on the bare word "id" was satisfied by the word "invalid" in
// the error's own prefix, so this pins wording long enough that only the intended
// message can match it.
func TestGrafanaRejectsMalformedEpochMillis(t *testing.T) {
	h, _, tn := newServer(t)
	rec := get(t, h, "/adapters/grafana/annotations?tenant="+tn+
		"&from=not-a-number&to=1789383600000")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "epoch milliseconds",
		"the response must name the expected format, not just echo the parameter name")
}

func TestOTLPAdapterUsesNanoseconds(t *testing.T) {
	h, s, tn := newServer(t)
	start := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	seed(t, s, tn, "dep-9", "deployment.started", start)

	rec := get(t, h, "/adapters/otlp/spans?tenant="+tn+
		"&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	span := onlySpan(t, envelope)
	assert.Equal(t, strconv.FormatInt(start.UnixNano(), 10), span["startTimeUnixNano"],
		"OTLP uses nanoseconds where Grafana uses milliseconds, encoded as a decimal "+
			"string — the unit and the string encoding both live in the adapter")
}

// onlySpan reaches through the OTLP envelope to the single span a test's fixture
// produces.
func onlySpan(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	resourceSpans, ok := envelope["resourceSpans"].([]any)
	require.True(t, ok, "resourceSpans must be a JSON array")
	require.Len(t, resourceSpans, 1)
	scopeSpans, ok := resourceSpans[0].(map[string]any)["scopeSpans"].([]any)
	require.True(t, ok, "scopeSpans must be a JSON array")
	require.Len(t, scopeSpans, 1)
	spans, ok := scopeSpans[0].(map[string]any)["spans"].([]any)
	require.True(t, ok, "spans must be a JSON array")
	require.Len(t, spans, 1)
	span, ok := spans[0].(map[string]any)
	require.True(t, ok)
	return span
}

// postJSON POSTs a JSON body, the way grafana/simple-json-datasource — an archived
// Grafana datasource plugin, but the only one that speaks this transport — queries
// annotations.
func postJSON(t *testing.T, h http.Handler, url, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

// annotationBody is a Grafana annotation-query POST body: the shape
// documented at https://github.com/grafana/simple-json-datasource#annotation-api,
// trimmed to the "range" field this adapter's contract reads.
func annotationBody(from, to time.Time) string {
	return fmt.Sprintf(`{"range":{"from":%q,"to":%q}}`,
		from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano))
}

// TestGrafanaAnnotationsPOSTUsesRequestBodyRange drives the
// new POST route with a Grafana-shaped JSON body and pins that the time range
// comes from range.from/range.to in the body, not from query parameters —
// the GET route's contract, which this route deliberately does not share.
func TestGrafanaAnnotationsPOSTUsesRequestBodyRange(t *testing.T) {
	h, s, tn := newServer(t)
	seed(t, s, tn, "dep-13", "deployment.started",
		time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC))

	body := annotationBody(
		time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC))
	rec := postJSON(t, h, "/adapters/grafana/annotations?tenant="+tn, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var anns []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anns))
	require.Len(t, anns, 1)
	assert.EqualValues(t, 1789380000000, anns[0]["time"])
}

// TestGrafanaAnnotationsPOSTRejectsMalformedBody pins the error path for both
// invalid JSON and a non-RFC3339 range value, naming the field so a caller
// can tell this apart from every other malformed-input error in this file.
func TestGrafanaAnnotationsPOSTRejectsMalformedBody(t *testing.T) {
	h, _, tn := newServer(t)

	rec := postJSON(t, h, "/adapters/grafana/annotations?tenant="+tn, `not json`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "not valid JSON")

	rec = postJSON(t, h, "/adapters/grafana/annotations?tenant="+tn,
		`{"range":{"from":"not-a-time","to":"2026-09-14T11:00:00Z"}}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "range.from",
		"the response must name the field, not just say the request was bad")
}

// TestGrafanaAnnotationsPOSTSignalsTruncation pins that the POST route signals
// truncation the same way every other read endpoint does. An endpoint that
// truncates silently hands back a short answer indistinguishable from a complete
// one, and adding a route to the read surface must not reintroduce that.
func TestGrafanaAnnotationsPOSTSignalsTruncation(t *testing.T) {
	h, s, tn := newServer(t)
	const seeded = 3
	seedManyChanges(t, s, tn, seeded)
	body := annotationBody(
		time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC))

	full := postJSON(t, h, fmt.Sprintf(
		"/adapters/grafana/annotations?tenant=%s&limit=%d", tn, seeded), body)
	require.Equal(t, http.StatusOK, full.Code, full.Body.String())
	assert.Empty(t, full.Header().Get(egress.TruncatedHeader),
		"a result of exactly the limit with nothing behind it is NOT truncated")

	cut := postJSON(t, h, fmt.Sprintf(
		"/adapters/grafana/annotations?tenant=%s&limit=%d", tn, seeded-1), body)
	require.Equal(t, http.StatusOK, cut.Code, cut.Body.String())
	assert.Equal(t, "true", cut.Header().Get(egress.TruncatedHeader),
		"rows were discarded and the caller has no other way to find out")
}

// closedTypeFor returns an event type unique to this call whose last dot-separated
// segment is "completed", so core.Fold gives the change a terminal status and an end.
//
// Both halves matter to TestGrafanaAnnotationsPOSTPathTenantIsScoped. Uniqueness is how
// a row is recognised in a Grafana annotation, which carries neither tenant nor ref: its
// title falls back to the event type. Terminal is what bounds the row in time — an open
// change's end_effective is core.OpenSentinel, so it would match every later query
// window however narrow, and rows seeded in tenant "default" outlive the test that wrote
// them.
func closedTypeFor(t *testing.T, suffix string) string {
	t.Helper()
	return fmt.Sprintf("horopter.scoped-%s-%s-%s.completed",
		procToken, strconv.FormatUint(seedSeq.Add(1), 36), suffix)
}

// annotationTitles returns the title of every annotation in a rendered response.
func annotationTitles(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var anns []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anns))
	titles := make([]string, 0, len(anns))
	for _, a := range anns {
		title, ok := a["title"].(string)
		require.True(t, ok, "every annotation must carry a string title")
		titles = append(titles, title)
	}
	return titles
}

// TestGrafanaAnnotationsPOSTPathTenantIsScoped pins that a POST to the path-scoped
// route queries that path's tenant, and that the pre-existing tenant-less route
// still resolves to DefaultTenant. The path is the mechanism a real Grafana can
// actually reach: both of its access modes discard a "?tenant=" query parameter
// baked into the configured datasource URL.
//
// The tenant-less leg used to assert the response was EMPTY, having seeded nothing in
// DefaultTenant. That is satisfied by every wrong answer as well as the right one: a
// route resolving to "", to a typo, or to a tenant that happens not to exist returns an
// empty list too. So DefaultTenant is now seeded with a row of its own and the assertion
// is that the response CONTAINS it — which only the correct resolution produces.
//
// Contains, never a count: this database is never truncated, so tenant "default"
// accumulates a row per execution, and no count of it is stable. The three rows share an
// instant taken from the wall clock and the window is a second either side of it, which
// keeps earlier runs' rows — terminal, so bounded by their own end — out of range and the
// result well clear of MaxLimit. A concurrent run landing inside that window adds rows
// this test simply does not name, which is why every assertion is by unique title.
func TestGrafanaAnnotationsPOSTPathTenantIsScoped(t *testing.T) {
	h, s, tnA := newServer(t)
	tnB := tnA + "-b"
	at := time.Now().UTC()
	typeA, typeB := closedTypeFor(t, "a"), closedTypeFor(t, "b")
	typeDefault := closedTypeFor(t, "default")
	seed(t, s, tnA, "dep-a", typeA, at)
	seed(t, s, tnB, "dep-b", typeB, at)
	seed(t, s, egress.DefaultTenant, "dep-default-"+procToken, typeDefault, at)

	body := annotationBody(at.Add(-time.Second), at.Add(time.Second))

	titlesA := annotationTitles(t, postJSON(t, h, "/adapters/grafana/"+tnA+"/annotations", body))
	assert.Equal(t, []string{typeA}, titlesA,
		"the path tenant must scope the query to tnA, not tnB or default")

	titlesB := annotationTitles(t, postJSON(t, h, "/adapters/grafana/"+tnB+"/annotations", body))
	assert.Equal(t, []string{typeB}, titlesB,
		"the path tenant must scope the query to tnB, not tnA or default")

	titlesDefault := annotationTitles(t, postJSON(t, h, "/adapters/grafana/annotations", body))
	assert.Contains(t, titlesDefault, typeDefault,
		"the tenant-less route must resolve to DefaultTenant; an empty result would also "+
			"be returned by a route resolving to \"\" or to a typo, so absence proves nothing")
	assert.NotContains(t, titlesDefault, typeA,
		"the tenant-less route must not leak tnA's rows")
	assert.NotContains(t, titlesDefault, typeB,
		"the tenant-less route must not leak tnB's rows")
}

// TestGrafanaAnnotationsPOSTRejectsOversizedBody pins that an unauthenticated
// caller must not be able to force an unbounded allocation through the first
// request-body route on egress's open surface.
func TestGrafanaAnnotationsPOSTRejectsOversizedBody(t *testing.T) {
	h, _, tn := newServer(t)

	oversized := `{"range":{"from":"` +
		strings.Repeat("x", egress.MaxAnnotationBodyBytes) + `","to":"2026-09-14T11:00:00Z"}}`
	rec := postJSON(t, h, "/adapters/grafana/annotations?tenant="+tn, oversized)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "byte limit")
}

func TestEgressRejectsMalformedTimeParams(t *testing.T) {
	h, _, tn := newServer(t)
	rec := get(t, h, "/v1/changes?tenant="+tn+"&from=not-a-time&to=2026-09-14T11:00:00Z")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "from")
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

// TestLimitParamRejectsInvalidValues exists because egress has no auth in v0.1:
// without a cap, GET /v1/events?tenant=X (no ref) or a wide GET /v1/changes window
// returns a tenant's entire log or change history to any caller, unbounded — both a
// resource-exhaustion and a full-log exfiltration path in a single request. A
// non-numeric or negative "limit" must be rejected outright rather than silently
// falling through to the store's own "0 means unlimited" convention.
func TestLimitParamRejectsInvalidValues(t *testing.T) {
	h, _, tn := newServer(t)
	base := "/v1/changes?tenant=" + tn + "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"

	rec := get(t, h, base+"&limit=not-a-number")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "between 1 and",
		"the response must name the accepted range")

	rec = get(t, h, base+"&limit=-5")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "between 1 and",
		"the response must name the accepted range")
}

// TestListEventsIsCappedAtMaxLimit seeds more rows than MaxLimit and checks both that
// the default (no "limit" parameter) truncates to it, and that a caller-supplied
// value above the cap is clamped rather than honored — the second case is what an
// unbounded-query mutation would restore.
func TestListEventsIsCappedAtMaxLimit(t *testing.T) {
	h, s, tn := newServer(t)
	seedManyEvents(t, s, tn, egress.MaxLimit+50)

	rec := get(t, h, "/v1/events?tenant="+tn)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var events []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &events))
	assert.Len(t, events, egress.MaxLimit, "the default limit must cap the result set")
	assert.Equal(t, "true", rec.Header().Get(egress.TruncatedHeader),
		"a result cut short at exactly the cap must say so; a caller cannot otherwise "+
			"distinguish it from a complete answer of exactly MaxLimit rows")

	recBig := get(t, h, "/v1/events?tenant="+tn+"&limit=5000")
	require.Equal(t, http.StatusOK, recBig.Code, recBig.Body.String())
	var eventsBig []map[string]any
	require.NoError(t, json.Unmarshal(recBig.Body.Bytes(), &eventsBig))
	assert.Len(t, eventsBig, egress.MaxLimit,
		"a caller-supplied limit above the cap must still be clamped to it")
	assert.Equal(t, "true", recBig.Header().Get(egress.TruncatedHeader))
}

// TestQueryChangesIsCappedAtMaxLimit is TestListEventsIsCappedAtMaxLimit's
// counterpart for /v1/changes, which reads through a separate store method
// (QueryChanges, not ListEvents) with its own independent LIMIT application — the two
// caps are not proven by one test alone.
func TestQueryChangesIsCappedAtMaxLimit(t *testing.T) {
	h, s, tn := newServer(t)
	seedManyChanges(t, s, tn, egress.MaxLimit+50)

	rec := get(t, h, "/v1/changes?tenant="+tn+
		"&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var changes []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &changes))
	assert.Len(t, changes, egress.MaxLimit, "the default limit must cap the result set")
	assert.Equal(t, "true", rec.Header().Get(egress.TruncatedHeader),
		"a result cut short at exactly the cap must say so")
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
	srv := egress.New(nil, fixedClock, egress.DefaultLimits())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "healthz must not require a database")
}

// readOnlyPOSTRoutes is TestNoWriteRouteIsRegisteredAnywhere's documented exception
// list to "every route is a GET": grafana/simple-json-datasource — an archived
// Grafana datasource plugin, but the only one that speaks this transport — POSTs
// the annotation query because the time range travels in a JSON request body, not
// because the route writes anything (see grafanaAnnotationsPOST's doc comment in
// server.go). Both the tenant-less and the path-scoped "/{tenant}/annotations"
// route reach that same read-only handler. Any other POST/PUT/DELETE pattern this
// test discovers is exactly the write-route regression it exists to catch.
var readOnlyPOSTRoutes = []string{
	"POST /adapters/grafana/annotations",
	"POST /adapters/grafana/{tenant}/annotations",
}

// routeHandleCall reports the mux.Handle/mux.HandleFunc pattern a call expression
// registers, and whether it is such a call at all. It exists so
// collectRoutePatterns's walk can filter with one call instead of the four chained
// type assertions that recognizing a route-registration call requires.
func routeHandleCall(t *testing.T, call *ast.CallExpr) (string, bool) {
	t.Helper()
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc" {
		return "", false
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok || recv.Name != "mux" {
		return "", false
	}
	require.NotEmpty(t, call.Args, "mux.%s call with no arguments", sel.Sel.Name)
	lit, ok := call.Args[0].(*ast.BasicLit)
	require.True(t, ok && lit.Kind == token.STRING,
		"mux.%s's first argument must be a string literal pattern", sel.Sel.Name)
	pattern, err := strconv.Unquote(lit.Value)
	require.NoError(t, err)
	return pattern, true
}

// collectRoutePatterns parses server.go and returns the pattern string registered by
// every mux.Handle/mux.HandleFunc call inside the Routes method — the route table
// TestNoWriteRouteIsRegisteredAnywhere checks.
func collectRoutePatterns(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err, "parse server.go to inspect Routes()")

	var patterns []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Routes" {
			return true
		}
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if pattern, matched := routeHandleCall(t, call); matched {
				patterns = append(patterns, pattern)
			}
			return true
		})
		return false
	})
	return patterns
}

// TestNoWriteRouteIsRegisteredAnywhere asserts, with a general structural check, that
// egress exposes no write route anywhere: it reads collectRoutePatterns's route table
// and asserts each pattern string starts with "GET " — except readOnlyPOSTRoutes,
// egress's deliberate read-only POSTs. A single HTTP probe against one path can only
// ever prove that one path has no write twin; a write route registered at any other
// path — POST /v1/ingest, PUT /v1/changes — would pass every HTTP-level test in this
// file while never being exercised. Reading the source is what generalizes the
// property to "no write route anywhere", not just "no write route at this one URL".
// It needs no database and runs unconditionally, like TestHealthzWorksWithNilStore.
func TestNoWriteRouteIsRegisteredAnywhere(t *testing.T) {
	patterns := collectRoutePatterns(t)

	require.NotEmpty(t, patterns, "Routes() must register at least one handler")
	for _, p := range patterns {
		if slices.Contains(readOnlyPOSTRoutes, p) {
			continue
		}
		assert.True(t, strings.HasPrefix(p, "GET "),
			"route pattern %q is not a GET route; egress must expose no write route", p)
	}
	for _, want := range readOnlyPOSTRoutes {
		assert.Contains(t, patterns, want,
			"the Grafana annotations POST routes must be registered exactly as documented")
	}
}

// TestTruncationIsSignalledOnEveryReadEndpoint drives both directions of the signal
// through all four read endpoints. Three of them read changes and one reads events, and
// two render vendor-owned shapes that could not carry an envelope flag — which is why the
// signal is a header, and why one endpoint's coverage says nothing about the other three.
//
// The untruncated case is the one that matters most: it asks for exactly as many rows as
// exist, which is precisely the case a "did we hit the limit?" count cannot tell apart
// from a truncated one. Only the over-fetch can, so a mechanism that marked every
// at-the-limit result as truncated would fail here.
func TestTruncationIsSignalledOnEveryReadEndpoint(t *testing.T) {
	h, s, tn := newServer(t)
	const seeded = 3
	seedManyChanges(t, s, tn, seeded)
	seedManyEvents(t, s, tn, seeded)

	window := "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	for name, base := range map[string]string{
		"/v1/changes":          "/v1/changes?tenant=" + tn + window,
		"/v1/events":           "/v1/events?tenant=" + tn,
		"/adapters/otlp/spans": "/adapters/otlp/spans?tenant=" + tn + window,
		"/adapters/grafana/annotations": "/adapters/grafana/annotations?tenant=" + tn +
			"&from=1789376400000&to=1789383600000",
	} {
		t.Run(name, func(t *testing.T) {
			full := get(t, h, fmt.Sprintf("%s&limit=%d", base, seeded))
			require.Equal(t, http.StatusOK, full.Code, full.Body.String())
			assert.Empty(t, full.Header().Get(egress.TruncatedHeader),
				"a result of exactly Limit rows with nothing behind it is NOT truncated; "+
					"marking it so would make the header meaningless")

			cut := get(t, h, fmt.Sprintf("%s&limit=%d", base, seeded-1))
			require.Equal(t, http.StatusOK, cut.Code, cut.Body.String())
			assert.Equal(t, "true", cut.Header().Get(egress.TruncatedHeader),
				"rows were discarded and the caller has no other way to find out")
		})
	}
}

// TestQueryChangesEmitsBothStartFacts pins the two keys migration 0003 split "start" into.
// The central argument for splitting them is that first_seen_at < started_at — an event
// claiming to predate the change's declared start — must be a QUERYABLE anomaly rather
// than one the fold destroyed. That is unreachable if the read side never emits the two
// facts, so the split buys nothing without these keys.
//
// The absent case is asserted as well as the present one: started_at missing means "no
// producer has asserted a start yet", which is a different fact from any timestamp, and a
// renderer that defaulted it to first_seen_at would put the inference back in the
// assertion's slot — the exact conflation 0003 exists to undo.
func TestQueryChangesEmitsBothStartFacts(t *testing.T) {
	h, s, tn := newServer(t)
	seen := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	asserted := time.Date(2026, 9, 14, 10, 20, 0, 0, time.UTC)
	url := "/v1/changes?tenant=" + tn + "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"

	only := func() map[string]any {
		rec := get(t, h, url)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var changes []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &changes))
		require.Len(t, changes, 1)
		return changes[0]
	}

	// An annotation only: evidence the change exists, with nothing asserting its start.
	seed(t, s, tn, "dep-facts", "deployment.annotated", seen)

	inferred := only()
	assert.Equal(t, seen.Format(time.RFC3339Nano), inferred["first_seen_at"],
		"the inference must be emitted, not just folded into start")
	assert.NotContains(t, inferred, "started_at",
		"no producer has asserted a start, and absent is the only honest rendering of that")
	assert.Equal(t, seen.Format(time.RFC3339Nano), inferred["start"],
		"start falls through to the inference while the assertion is NULL")

	// Now a start event asserting a LATER time than the earliest evidence.
	seed(t, s, tn, "dep-facts", "deployment.started", asserted)

	got := only()
	assert.Equal(t, asserted.Format(time.RFC3339Nano), got["started_at"],
		"the assertion must be emitted once a producer makes one")
	assert.Equal(t, seen.Format(time.RFC3339Nano), got["first_seen_at"],
		"the earlier evidence must survive; first_seen_at < started_at is the anomaly "+
			"this pair exists to make queryable")
	assert.Equal(t, asserted.Format(time.RFC3339Nano), got["start"],
		"start follows the assertion once there is one")
}

// TestReadyzReports503WhenTheDatabaseIsUnreachable is the half of the readiness contract
// that matters. A readiness probe exists to remove a pod from the load balancer, so a
// readyz that only ever answers 200 is worse than none: it keeps a daemon that cannot
// reach its database in rotation, answering every query with a 500. The happy path above
// cannot catch that — a handler that returned 200 unconditionally passes it.
//
// Closing the pool is a real database outage rather than a faked error: Ping then fails
// the way it fails when Postgres is gone, which is the case readyz exists for.
// pgxpool.Close is guarded by a sync.Once, so newServer's own t.Cleanup close is safe.
func TestReadyzReports503WhenTheDatabaseIsUnreachable(t *testing.T) {
	h, s, _ := newServer(t)
	s.Close()

	rec := get(t, h, "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "database unreachable",
		"an operator reading the probe's body must be told what is wrong")
}

// TestReadyzDoesNotRelayTheDatabaseError pins what the 503 body withholds. The probe is
// unauthenticated — an orchestrator has no credential to send — and pgx formats a failed
// connection with the database user, database name, host and port, so relaying the error
// handed all four to anyone who could reach the port during an outage.
func TestReadyzDoesNotRelayTheDatabaseError(t *testing.T) {
	h, s, _ := newServer(t)
	s.Close()

	rec := get(t, h, "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "closed pool",
		"the database error must not reach an unauthenticated caller")
}

// TestFailedReadinessProbeLogsItsCause is the other half: withholding the error from the
// body must not lose it. A pod that drops out of rotation with no record of why leaves an
// operator reproducing the probe by hand to find out.
func TestFailedReadinessProbeLogsItsCause(t *testing.T) {
	h, s, _ := newServer(t)
	s.Close()
	records := captureLogs(t)

	rec := get(t, h, "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())

	logged := slices.ContainsFunc(records(), func(r map[string]any) bool {
		cause, _ := r["error"].(string)
		return r["level"] == "ERROR" && strings.Contains(cause, "closed pool")
	})
	assert.True(t, logged, "the probe's failure must reach the server log; got %v", records())
}

// TestEgressRejectsAnInvertedWindow pins from > to as the caller's error on every window
// parser egress has. It was accepted: the overlap predicate is
// "start_at <= :to AND end_effective >= :from", so an inverted window does not return
// nothing, it returns the changes SPANNING the interval the caller inverted — a
// plausible-looking answer to a question nobody asked.
//
// All four transports are driven because the rule lives in newWindow and each parser has
// to route through it; a parser that builds a window literal instead would pass every
// other test in this file.
func TestEgressRejectsAnInvertedWindow(t *testing.T) {
	h, _, tn := newServer(t)
	early := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	late := time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC)
	inverted := "&from=" + late.Format(time.RFC3339) + "&to=" + early.Format(time.RFC3339)

	for name, url := range map[string]string{
		"/v1/changes":          "/v1/changes?tenant=" + tn + inverted,
		"/adapters/otlp/spans": "/adapters/otlp/spans?tenant=" + tn + inverted,
		"GET /adapters/grafana/annotations": fmt.Sprintf(
			"/adapters/grafana/annotations?tenant=%s&from=%d&to=%d",
			tn, late.UnixMilli(), early.UnixMilli()),
	} {
		t.Run(name, func(t *testing.T) {
			rec := get(t, h, url)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "earliest first",
				"the message must say what the caller should change about its own request")
		})
	}

	t.Run("POST /adapters/grafana/annotations", func(t *testing.T) {
		rec := postJSON(t, h, "/adapters/grafana/annotations?tenant="+tn,
			annotationBody(late, early))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "earliest first")
	})
}

// TestGrafanaRejectsAnOutOfRangeEpochWindow pins the epoch bound. epochMillisParam
// accepted any int64, and the failure that produced was silent rather than loud:
// to=9223372036854775807 is a year-292278994 time.Time, pgx encodes timestamptz as
// microseconds since 2000-01-01 in an int64, and that overflows — measured on this stack
// it binds as 1969-12-31T23:59:59.999Z. The endpoint answered 200 with an empty list, so
// a caller asking for everything up to the end of time was told there was nothing, with
// no error raised anywhere for an operator to see. The assertions check both halves: the
// status is the caller's own 400, and the body names the caller's parameter rather than
// our store.
func TestGrafanaRejectsAnOutOfRangeEpochWindow(t *testing.T) {
	h, _, tn := newServer(t)
	for name, url := range map[string]string{
		"to overflows int64": "/adapters/grafana/annotations?tenant=" + tn +
			"&from=0&to=9223372036854775807",
		"from predates the epoch": "/adapters/grafana/annotations?tenant=" + tn +
			"&from=-1&to=1789383600000",
	} {
		t.Run(name, func(t *testing.T) {
			rec := get(t, h, url)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "out of range",
				"the caller must be told its own value was rejected")
			assert.NotContains(t, strings.ToLower(rec.Body.String()), "postgres",
				"a bad window must never surface store detail to an unauthenticated caller")
		})
	}
}

// recordSpans installs a recording provider and returns it with a restore function.
//
// This is a copy of internal/store/postgres/append_test.go's helper of the same name,
// not an import: that file is package postgres_test and this is package egress_test, so
// it cannot be imported. The restore is best-effort and deliberately named for what it
// attempts rather than what it guarantees — see the postgres copy for why.
func recordSpans(t *testing.T) (*tracetest.SpanRecorder, func()) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	return rec, func() { otel.SetTracerProvider(prev) }
}

// spanAttrValues returns key's value from every ended span named spanName, in the order
// the spans ended. A span lacking the attribute is SKIPPED rather than contributing an
// empty entry. Pass "" as spanName to match every span. kv.Value.String() is used rather
// than the deprecated Emit(), which staticcheck rejects under SA1019 on the pinned otel
// v1.46.0.
func spanAttrValues(rec *tracetest.SpanRecorder, spanName, key string) []string {
	var out []string
	for _, span := range rec.Ended() {
		if spanName != "" && span.Name() != spanName {
			continue
		}
		for _, kv := range span.Attributes() {
			if string(kv.Key) == key {
				out = append(out, kv.Value.String())
			}
		}
	}
	return out
}

// TestGrafanaPathTenantIsRecordedAsPathSourced pins the source tenantOf reports, driven
// through the one route that can actually produce "path": the path-scoped Grafana
// annotations route.
func TestGrafanaPathTenantIsRecordedAsPathSourced(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()

	// No seeding. grafana.Renderer.Render builds an empty slice and returns it, so an
	// empty result still renders 200 with []. The assertion is about which mechanism
	// supplied the tenant, not about any row coming back — and seeding under newServer's
	// tenant would write to the wrong one anyway, since the path segment "acme" is the
	// whole point. Discard all three results: a bound-but-unused tn is a compile error.
	h, _, _ := newServer(t)
	body := `{"range":{"from":"2026-09-15T09:00:00.000Z","to":"2026-09-15T11:00:00.000Z"}}`
	resp := postJSON(t, h, "/adapters/grafana/acme/annotations", body)
	require.Equal(t, http.StatusOK, resp.Code)

	sources := spanAttrValues(rec, "", "horopter.tenant.source")
	assert.Contains(t, sources, "path",
		"a path-scoped route must record that the path supplied the tenant")
}

// TestFailedRenderStillRecordsTheAdapter is the failure half of horopter.adapter. Which
// vendor shape a request was for is known before the query runs, and a failing query is
// exactly when it matters — "Grafana's annotation calls are erroring" and "the OTLP span
// endpoint is erroring" are different incidents. Recording it after the query succeeded
// left every 500 unattributed.
//
// Closing the store is what makes the query fail without reaching into the handler:
// pgxpool.Close is idempotent, so newServer's own t.Cleanup(s.Close) still runs safely.
func TestFailedRenderStillRecordsTheAdapter(t *testing.T) {
	rec, restore := recordSpans(t)
	defer restore()

	h, s, _ := newServer(t)
	s.Close()

	body := `{"range":{"from":"2026-09-15T09:00:00.000Z","to":"2026-09-15T11:00:00.000Z"}}`
	resp := postJSON(t, h, "/adapters/grafana/annotations", body)
	require.Equal(t, http.StatusInternalServerError, resp.Code,
		"a closed pool must fail the query; a 200 here means the test proved nothing")

	assert.Contains(t, spanAttrValues(rec, "", "horopter.adapter"), "grafana",
		"a failed render must still say which adapter asked")
}

// readRoute is one request against one egress read route, naming the pattern Routes
// registers it under so TestEveryReadRouteHasAFailureCase can match the two up.
type readRoute struct {
	pattern, method, url, body string
}

// readRoutes returns one well-formed request per egress read route, each naming tenant
// tn: through the path on the path-scoped Grafana route, through the query string on the
// rest. Every request is valid, so a failure can only come from the store.
func readRoutes(tn string) []readRoute {
	const rfc3339Window = "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	const annotationWindow = `{"range":{"from":"2026-09-15T09:00:00.000Z",` +
		`"to":"2026-09-15T11:00:00.000Z"}}`
	return []readRoute{
		{"GET /v1/changes", http.MethodGet, "/v1/changes?tenant=" + tn + rfc3339Window, ""},
		{"GET /v1/events", http.MethodGet, "/v1/events?tenant=" + tn, ""},
		{"GET /adapters/grafana/annotations", http.MethodGet,
			"/adapters/grafana/annotations?tenant=" + tn + "&from=1789376400000&to=1789383600000",
			""},
		{"POST /adapters/grafana/annotations", http.MethodPost,
			"/adapters/grafana/annotations?tenant=" + tn, annotationWindow},
		{"POST /adapters/grafana/{tenant}/annotations", http.MethodPost,
			"/adapters/grafana/" + tn + "/annotations", annotationWindow},
		{"GET /adapters/otlp/spans", http.MethodGet,
			"/adapters/otlp/spans?tenant=" + tn + rfc3339Window, ""},
	}
}

func (rr readRoute) serve(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	if rr.method == http.MethodPost {
		return postJSON(t, h, rr.url, rr.body)
	}
	return get(t, h, rr.url)
}

// captureLogs sends slog's default logger to a buffer until the test ends, and returns a
// function decoding every record written so far. Swapping a process-wide default is safe
// only because no test in this package calls t.Parallel.
func captureLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() []map[string]any {
		var records []map[string]any
		dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
		for dec.More() {
			var record map[string]any
			require.NoError(t, dec.Decode(&record))
			records = append(records, record)
		}
		return records
	}
}

// TestEveryReadRouteHasAFailureCase keeps readRoutes complete. The two failure-policy
// tests below drive every route in it, so a read route registered in Routes but missing
// here would be one whose failures nobody checks are logged or withheld — exactly how all
// four read paths came to fail silently. The probes are excluded: they read no tenant's
// data and have their own failure tests.
func TestEveryReadRouteHasAFailureCase(t *testing.T) {
	var covered []string
	for _, route := range readRoutes("any") {
		covered = append(covered, route.pattern)
	}
	var registered []string
	for _, p := range collectRoutePatterns(t) {
		if p != "GET /healthz" && p != "GET /readyz" {
			registered = append(registered, p)
		}
	}
	assert.ElementsMatch(t, registered, covered,
		"readRoutes must list every read route Routes registers, and nothing else")
}

// TestEveryFailedReadIsLoggedWithItsCause pins the server-side half of egress's failure
// policy. Every read route used to answer a store failure with a 500 and log nothing, so
// an operator watching an hour of blank Grafana panels found two startup lines in the
// daemon's output and no record of why; the only copy of the cause went to a caller the
// operator does not control. Tracing is no substitute: it is off by default.
//
// The tenant is asserted with the cause because which tenant's reads are failing is the
// first thing an operator needs from the line.
func TestEveryFailedReadIsLoggedWithItsCause(t *testing.T) {
	h, s, tn := newServer(t)
	s.Close()

	for _, route := range readRoutes(tn) {
		t.Run(route.pattern, func(t *testing.T) {
			records := captureLogs(t)
			rec := route.serve(t, h)
			require.Equal(t, http.StatusInternalServerError, rec.Code,
				"a closed pool must fail the read; anything else means the test proved nothing")

			logged := slices.ContainsFunc(records(), func(r map[string]any) bool {
				cause, _ := r["error"].(string)
				return r["level"] == "ERROR" && r["tenant"] == tn &&
					strings.Contains(cause, "closed pool")
			})
			assert.True(t, logged,
				"the store's error and the tenant must reach the server log; got %v", records())
		})
	}
}

// TestFailedReadDoesNotRelayStoreErrorText pins the caller-facing half. The store's error
// names tables, columns and constraints — and for a failed connection the database host,
// user and name — and egress is unauthenticated, so relaying it published that detail to
// anyone who could reach the port. The wrapped error also carried the caller's own tenant
// string, making every failing route a reflector for caller-chosen text. Ingress's
// appendFailed withholds the same text for the same reason.
func TestFailedReadDoesNotRelayStoreErrorText(t *testing.T) {
	h, s, tn := newServer(t)
	s.Close()

	for _, route := range readRoutes(tn) {
		t.Run(route.pattern, func(t *testing.T) {
			rec := route.serve(t, h)
			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "closed pool",
				"the store's error text must not reach the caller")
			assert.NotContains(t, rec.Body.String(), tn,
				"the caller's tenant must not be reflected back in an error body")
		})
	}
}

// seedWith appends an event with an explicit subject, so a filter test can tell the
// subject and type columns apart. seed() hardcodes one subject for every event, so a
// test built on it cannot distinguish filtering by subject from filtering by type.
func seedWith(t *testing.T, s *postgres.Store, tn, ref, typ, subject string, at time.Time) {
	t.Helper()
	id := core.NewULID(time.Now(), rand.Reader)
	_, _, err := s.AppendEvent(context.Background(), core.Event{
		ID: id, Tenant: tn, IdempotencyKey: id, SchemaVersion: 1,
		Source: "ci", Type: typ, Subject: subject, Actor: "ci-bot",
		OccurredAt: at, ReceivedAt: at, CorrelationRef: ref,
		Attributes: map[string]any{},
	})
	require.NoError(t, err)
}

// refsIn returns the refs of a /v1/changes response, for comparing result sets.
func refsIn(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var changes []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &changes))
	refs := make([]string, 0, len(changes))
	for _, c := range changes {
		refs = append(refs, c["ref"].(string))
	}
	sort.Strings(refs)
	return refs
}

// The subject, type and status query parameters had no end-to-end coverage: the store
// tests call QueryChanges with a hand-built struct and so skip the URL-to-struct wiring
// entirely. A transposition there — Subject reading "type", Type reading "subject" — is
// invisible to every other test, and it is the mechanism a Grafana dashboard uses to
// scope a query.
//
// The two changes differ in BOTH subject and type, so a swap makes each filter select
// nothing rather than selecting the wrong row. A fixture sharing one subject cannot tell
// the two columns apart at all.
func TestQueryChangesFiltersBySubjectAndType(t *testing.T) {
	h, s, tn := newServer(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	seedWith(t, s, tn, "dep-90", "deployment.started", "acme/prod", at)
	seedWith(t, s, tn, "inc-90", "incident.declared", "acme/staging", at)

	window := "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	assert.Equal(t, []string{"dep-90", "inc-90"},
		refsIn(t, get(t, h, "/v1/changes?tenant="+tn+window)),
		"precondition: both changes are in the window")

	assert.Equal(t, []string{"dep-90"},
		refsIn(t, get(t, h, "/v1/changes?tenant="+tn+window+"&subject=acme/prod")),
		"subject= must filter on subject, not on type")
	assert.Equal(t, []string{"inc-90"},
		refsIn(t, get(t, h, "/v1/changes?tenant="+tn+window+"&type=incident.declared")),
		"type= must filter on type, not on subject")
}

// status= is wired separately and is dropped just as silently: without it the endpoint
// answers every status query with the unfiltered set, so a dashboard asking for failures
// is handed everything and shows no error.
func TestQueryChangesFiltersByStatus(t *testing.T) {
	h, s, tn := newServer(t)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	seedWith(t, s, tn, "dep-91", "deployment.started", "acme/prod", at)
	seedWith(t, s, tn, "dep-92", "deployment.started", "acme/prod", at)
	seedWith(t, s, tn, "dep-92", "deployment.completed", "acme/prod", at.Add(time.Minute))

	window := "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	assert.Equal(t, []string{"dep-91", "dep-92"},
		refsIn(t, get(t, h, "/v1/changes?tenant="+tn+window)),
		"precondition: both changes are in the window")

	assert.Equal(t, []string{"dep-92"},
		refsIn(t, get(t, h, "/v1/changes?tenant="+tn+window+"&status=completed")),
		"status= must narrow the result set")
	assert.Equal(t, []string{"dep-91"},
		refsIn(t, get(t, h, "/v1/changes?tenant="+tn+window+"&status=in_progress")),
		"the complementary status must select the other change")
}
