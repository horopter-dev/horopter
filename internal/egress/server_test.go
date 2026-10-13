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
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/egress"
	"horopter.dev/horopter/internal/store/postgres"
	"horopter.dev/horopter/internal/testdb"
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
// default — under limits, authenticating through a fresh Authenticator over that same store. The
// concurrency limit starts at the pool size, so a small pool is how a test lets one held read
// fill it.
func newServerWith(
	t *testing.T, poolMaxConns int, limits egress.Limits,
) (*egress.Server, *postgres.Store, string) {
	t.Helper()
	s, tn := openTestStore(t, poolMaxConns)
	authn := auth.New(auth.NewStore(s.Pool()), fixedClock, auth.DefaultOptions())
	srv, err := egress.New(s, fixedClock, limits, authn)
	require.NoError(t, err)
	return srv, s, tn
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
	return s, testdb.NewTenant(t, tn)
}

func fixedClock() time.Time { return fixed }

// seedCredential inserts a principal and a credential of role, scoped to tenantID ("" for a
// global credential), registering cleanup for both, and returns the plaintext key SplitKey
// resolves. Every egress test now needs a credential: every non-probe route requires one.
//
// The insert goes through s's own pool, live for the call; the cleanup goes through
// testdb's shared pool instead, since several tests in this package close s's pool before
// the test ends, and a cleanup racing that closed pool would silently leave the row behind
// for testdb's own tenant cleanup to trip over as a foreign-key violation.
func seedCredential(t *testing.T, s *postgres.Store, role, tenantID string) string {
	t.Helper()
	key, prefix, hash, err := auth.NewKey(rand.Reader)
	require.NoError(t, err)

	principalID := "prn_test_" + prefix
	credentialID := "cred_test_" + prefix
	ctx := context.Background()

	_, err = s.Pool().Exec(ctx, `INSERT INTO principal (id, display_name) VALUES ($1, $1)`,
		principalID)
	require.NoError(t, err)
	t.Cleanup(func() { deleteRow(t, "principal", principalID) })

	var tenant *string
	if tenantID != "" {
		tenant = &tenantID
	}
	_, err = s.Pool().Exec(ctx, `
		INSERT INTO credential (id, principal_id, type, key_prefix, key_hash, role,
		                        tenant_id, source, locus)
		VALUES ($1, $2, 'machine_key', $3, $4, $5, $6, 'test', 'test')`,
		credentialID, principalID, prefix, hash, role, tenant)
	require.NoError(t, err)
	t.Cleanup(func() { deleteRow(t, "credential", credentialID) })

	return key
}

// deleteRow removes id from table through testdb's shared, never-closed pool. Failures are
// logged, not fatal, matching testdb's own cleanup helper: residue is exactly the condition
// being removed, so failing an otherwise passing test over it would trade a tidy database
// for a flaky suite.
func deleteRow(t *testing.T, table, id string) {
	t.Helper()
	pool, err := testdb.Pool()
	if err != nil {
		t.Logf("%s cleanup for %q: open pool: %v", table, id, err)
		return
	}
	if _, err := pool.Exec(context.Background(),
		"DELETE FROM "+table+" WHERE id = $1", id); err != nil {
		t.Logf("%s cleanup for %q: %v", table, id, err)
	}
}

// readerKey mints a reader credential scoped to tn on s. It is the credential most tests in
// this file drive requests with, since most of what egress serves is scoped to one tenant.
func readerKey(t *testing.T, s *postgres.Store, tn string) string {
	t.Helper()
	return seedCredential(t, s, "reader", tn)
}

// warmCredentialCache issues one authenticated request so key's credential is cached before
// the caller does whatever it means to measure or constrain next: an uncached lookup's own
// database round trip — or, against a pool this call is about to exhaust or close, its
// failure — would otherwise land on that measurement instead of on this throwaway read.
//
// This file is compiled under -race, unlike expansion_test.go: keep this here, not there, or
// a race build fails on every caller outside that file.
func warmCredentialCache(t *testing.T, h http.Handler, key, tn string) {
	t.Helper()
	rec := get(t, h, key, "/v1/changes?tenant="+tn+
		"&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

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
		ID: id, Tenant: tn, IdempotencyKey: id, SchemaVersion: core.CurrentSchemaVersion,
		Source: "ci", Type: typ, Subject: "acme/prod", Actor: "ci-bot",
		OccurredAt: at, ReceivedAt: at, CorrelationRef: core.RefFromRow(ref),
		Attributes: map[string]any{"service.version": "2.5.0"}, Origin: core.OriginProcess,
		Locus: core.LocusFromRow("ci"),
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
			                   correlation_ref, attributes, locus)
			VALUES ($1,$2,$3,1,'ci','deployment.started','acme/prod','ci-bot','',
			        $4,$4,'ci/bulk-ref','{}'::jsonb,'ci')`,
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
		ref := fmt.Sprintf("ci/bulk-%s-%s-%d", t.Name(), procToken, tenantSeq.Add(1))
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

// get drives an authenticated GET against h, sending key as a bearer credential.
func get(t *testing.T, h http.Handler, key, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// getUnauthenticated drives a request against h with no Authorization header. route is a
// registered pattern, "METHOD /path" as routesOf and collectRoutePatterns report it — not a
// full URL — so the method is split off before the request is built.
func getUnauthenticated(t *testing.T, h http.Handler, route string) *httptest.ResponseRecorder {
	t.Helper()
	method, path, ok := strings.Cut(route, " ")
	require.True(t, ok, "route pattern %q must be \"METHOD /path\"", route)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// postTo drives an authenticated POST against h, with no body: every use of this in the
// package drives a route that either no longer exists or never reads one.
func postTo(t *testing.T, h http.Handler, key, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// routesOf enumerates every route Routes registers, read from server.go's own AST via
// collectRoutePatterns: net/http's ServeMux has no method that lists its own registrations,
// so this reads the source instead of hand-maintaining a duplicate list. h is unused; it is
// a parameter so a call at the point of use reads like it is asking the running server.
func routesOf(t *testing.T, _ http.Handler) []string {
	t.Helper()
	return collectRoutePatterns(t)
}

// isProbe reports whether route is one of the two unauthenticated liveness/readiness
// probes.
func isProbe(route string) bool {
	return route == "GET /healthz" || route == "GET /readyz"
}

// admittedCount reports how many reads srv's concurrency limiter currently holds a permit
// for.
func admittedCount(t *testing.T, srv *egress.Server) int {
	t.Helper()
	return egress.InFlight(srv)
}

// jsonChange is the subset of a rendered /v1/changes entry the tests in this file read
// back.
type jsonChange struct {
	Ref string `json:"ref"`
}

// changesOf decodes a /v1/changes response body.
func changesOf(t *testing.T, rec *httptest.ResponseRecorder) []jsonChange {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Changes []jsonChange `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Changes
}

func TestQueryChangesReturnsNeutralJSON(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	seed(t, s, tn, "ci/dep-7", "deployment.started",
		time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC))

	rec := get(t, h, key, "/v1/changes?tenant="+tn+
		"&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	changes := changesBody(t, rec)
	require.Len(t, changes, 1)
	assert.Equal(t, "ci/dep-7", changes[0]["ref"])
	assert.Equal(t, "in_progress", changes[0]["status"])
	assert.NotContains(t, changes[0], "timeEnd",
		"the neutral API must not leak a vendor field name")
}

func TestGrafanaAdapterUsesEpochMillis(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	seed(t, s, tn, "ci/dep-8", "deployment.started",
		time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC))

	rec := get(t, h, key, "/adapters/grafana/annotations?tenant="+tn+
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
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	rec := get(t, h, key, "/adapters/grafana/annotations?tenant="+tn+
		"&from=not-a-number&to=1789383600000")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "epoch milliseconds",
		"the response must name the expected format, not just echo the parameter name")
}

func TestOTLPAdapterUsesNanoseconds(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	start := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	seed(t, s, tn, "ci/dep-9", "deployment.started", start)

	rec := get(t, h, key, "/adapters/otlp/spans?tenant="+tn+
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

func TestEgressRejectsMalformedTimeParams(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	rec := get(t, h, key, "/v1/changes?tenant="+tn+"&from=not-a-time&to=2026-09-14T11:00:00Z")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "from")
}

func TestListEventsReturnsRawLog(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	seed(t, s, tn, "ci/dep-10", "deployment.started",
		time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC))
	seed(t, s, tn, "", "config.changed",
		time.Date(2026, 9, 14, 10, 5, 0, 0, time.UTC))

	rec := get(t, h, key, "/v1/events?tenant="+tn+"&ref="+url.QueryEscape("ci/dep-10"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	events := eventsBody(t, rec)
	require.Len(t, events, 1)
	assert.Equal(t, "ci/dep-10", events[0]["correlation_ref"])
	assert.Equal(t, "deployment.started", events[0]["type"])

	// The second seed() call above passes ref "", which stores CorrelationRef as the zero
	// core.Ref — a point event. Every other seed() call in this suite passes a non-empty
	// ref, so nothing else exercises how a point event's correlation_ref renders. It is
	// fetched here with no ref filter, since a point's projection ref is <locus>/<its own
	// event id>, which this test never captures on its own.
	full := get(t, h, key, "/v1/events?tenant="+tn)
	require.Equal(t, http.StatusOK, full.Code, full.Body.String())
	all := eventsBody(t, full)
	var point map[string]any
	for _, e := range all {
		if e["type"] == "config.changed" {
			point = e
		}
	}
	require.NotNil(t, point, "the seeded point event must appear in the unscoped log")
	assert.Equal(t, "", point["correlation_ref"],
		"a point event's correlation_ref must render as the empty string, not \"/\"")
}

// TestLimitParamRejectsInvalidValues pins that a non-numeric or negative "limit" is
// rejected outright rather than silently falling through to the store's own "0 means
// unlimited" convention — a reader credential can already reach a tenant's whole change
// history in one request if MaxLimit is not enforced.
func TestLimitParamRejectsInvalidValues(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	base := "/v1/changes?tenant=" + tn + "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"

	rec := get(t, h, key, base+"&limit=not-a-number")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "between 1 and",
		"the response must name the accepted range")

	rec = get(t, h, key, base+"&limit=-5")
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
	key := readerKey(t, s, tn)
	seedManyEvents(t, s, tn, egress.MaxLimit+50)

	rec := get(t, h, key, "/v1/events?tenant="+tn)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	events := eventsBody(t, rec)
	assert.Len(t, events, egress.MaxLimit, "the default limit must cap the result set")
	assert.Equal(t, "true", rec.Header().Get(egress.TruncatedHeader),
		"a result cut short at exactly the cap must say so; a caller cannot otherwise "+
			"distinguish it from a complete answer of exactly MaxLimit rows")

	recBig := get(t, h, key, "/v1/events?tenant="+tn+"&limit=5000")
	require.Equal(t, http.StatusOK, recBig.Code, recBig.Body.String())
	eventsBig := eventsBody(t, recBig)
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
	key := readerKey(t, s, tn)
	seedManyChanges(t, s, tn, egress.MaxLimit+50)

	rec := get(t, h, key, "/v1/changes?tenant="+tn+
		"&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	changes := changesBody(t, rec)
	assert.Len(t, changes, egress.MaxLimit, "the default limit must cap the result set")
	assert.Equal(t, "true", rec.Header().Get(egress.TruncatedHeader),
		"a result cut short at exactly the cap must say so")
}

// TestReadyzReportsReadyWithLiveDatabase and its 503 counterparts below drive the probe
// with no Authorization header, since an orchestrator holds no credential to send — probes
// stay reachable without one.
func TestReadyzReportsReadyWithLiveDatabase(t *testing.T) {
	h, _, _ := newServer(t)
	rec := getUnauthenticated(t, h, "GET /readyz")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestHealthzWorksWithNilStore proves healthz never touches the database, without
// needing a live one: New does not dereference a nil store — concurrencyBounds
// special-cases nil rather than calling its Pool() — and a Server built with a nil
// store still has to answer 200 from healthz for that to hold. It runs
// unconditionally — no HOROPTER_TEST_DSN skip, and driven through the real Routes()
// mux rather than calling the handler directly — because it is the one property in
// this suite that a database-less CI leg can actually exercise: every other test here
// skips without a live Postgres, so without this one the liveness/readiness split
// would have zero coverage in that environment. If a future change reintroduces a
// Pool().Ping call into healthz, this test panics on the nil store and fails.
//
// The authenticator's own lookup is nil too: healthz is registered unwrapped, so nothing
// ever calls it.
func TestHealthzWorksWithNilStore(t *testing.T) {
	authn := auth.New(nil, fixedClock, auth.DefaultOptions())
	srv, err := egress.New(nil, fixedClock, egress.DefaultLimits(), authn)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "healthz must not require a database")
}

// TestNewRejectsANilAuthenticator pins the guard where it is structural rather than
// remembered: every non-probe route is gated on the authenticator, so a Server built
// without one would either panic on the first request or need every caller to remember
// the check itself. It runs unconditionally: a constructor that refuses its argument
// needs no database.
func TestNewRejectsANilAuthenticator(t *testing.T) {
	srv, err := egress.New(nil, fixedClock, egress.DefaultLimits(), nil)
	require.Error(t, err, "a nil authenticator would panic on the first request rather "+
		"than reject it")
	assert.Nil(t, srv, "a rejected configuration must not yield a usable server")
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
// TestEveryNonProbeEgressRouteRequiresACredential and TestNoWriteRouteIsRegisteredAnywhere
// check.
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
// egress exposes no write route anywhere: every pattern collectRoutePatterns finds must
// start with "GET ". A single HTTP probe against one path can only ever prove that one
// path has no write twin; a write route registered at any other path — POST
// /v1/ingest, PUT /v1/changes — would pass every HTTP-level test in this file while
// never being exercised. Reading the source is what generalizes the property to "no
// write route anywhere", not just "no write route at this one URL".
// It needs no database and runs unconditionally, like TestHealthzWorksWithNilStore.
func TestNoWriteRouteIsRegisteredAnywhere(t *testing.T) {
	patterns := collectRoutePatterns(t)

	require.NotEmpty(t, patterns, "Routes() must register at least one handler")
	for _, p := range patterns {
		assert.True(t, strings.HasPrefix(p, "GET "),
			"route pattern %q is not a GET route; egress must expose no write route", p)
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
	key := readerKey(t, s, tn)
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
			full := get(t, h, key, fmt.Sprintf("%s&limit=%d", base, seeded))
			require.Equal(t, http.StatusOK, full.Code, full.Body.String())
			assert.Empty(t, full.Header().Get(egress.TruncatedHeader),
				"a result of exactly Limit rows with nothing behind it is NOT truncated; "+
					"marking it so would make the header meaningless")

			cut := get(t, h, key, fmt.Sprintf("%s&limit=%d", base, seeded-1))
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
	key := readerKey(t, s, tn)
	seen := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	asserted := time.Date(2026, 9, 14, 10, 20, 0, 0, time.UTC)
	url := "/v1/changes?tenant=" + tn + "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"

	only := func() map[string]any {
		rec := get(t, h, key, url)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		changes := changesBody(t, rec)
		require.Len(t, changes, 1)
		return changes[0]
	}

	// An annotation only: evidence the change exists, with nothing asserting its start.
	seed(t, s, tn, "ci/dep-facts", "deployment.annotated", seen)

	inferred := only()
	assert.Equal(t, seen.Format(time.RFC3339Nano), inferred["first_seen_at"],
		"the inference must be emitted, not just folded into start")
	assert.NotContains(t, inferred, "started_at",
		"no producer has asserted a start, and absent is the only honest rendering of that")
	assert.Equal(t, seen.Format(time.RFC3339Nano), inferred["start"],
		"start falls through to the inference while the assertion is NULL")

	// Now a start event asserting a LATER time than the earliest evidence.
	seed(t, s, tn, "ci/dep-facts", "deployment.started", asserted)

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

	rec := getUnauthenticated(t, h, "GET /readyz")
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

	rec := getUnauthenticated(t, h, "GET /readyz")
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

	rec := getUnauthenticated(t, h, "GET /readyz")
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
// Both transports are driven because the rule lives in newWindow and each parser has to
// route through it; a parser that built a window literal instead would pass every other
// test in this file.
func TestEgressRejectsAnInvertedWindow(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
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
			rec := get(t, h, key, url)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "earliest first",
				"the message must say what the caller should change about its own request")
		})
	}
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
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	for name, url := range map[string]string{
		"to overflows int64": "/adapters/grafana/annotations?tenant=" + tn +
			"&from=0&to=9223372036854775807",
		"from predates the epoch": "/adapters/grafana/annotations?tenant=" + tn +
			"&from=-1&to=1789383600000",
	} {
		t.Run(name, func(t *testing.T) {
			rec := get(t, h, key, url)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "out of range",
				"the caller must be told its own value was rejected")
			assert.NotContains(t, strings.ToLower(rec.Body.String()), "postgres",
				"a bad window must never surface store detail, even to an authenticated caller")
		})
	}
}

// TestGrafanaAcceptsTheEpochBoundsThemselves pins the other side of the bound: the epoch
// itself and the open-ended sentinel are valid instants, and a dashboard asking for
// "everything" sends exactly those. Making either comparison strict refuses them as out of
// range, which only a test at the bound can tell from a correct check.
func TestGrafanaAcceptsTheEpochBoundsThemselves(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	url := fmt.Sprintf("/adapters/grafana/annotations?tenant=%s&from=0&to=%d",
		tn, core.OpenSentinel.UnixMilli())
	rec := get(t, h, key, url)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
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

// TestFailedRenderStillRecordsTheAdapter is the failure half of horopter.adapter. Which
// vendor shape a request was for is known before the query runs, and a failing query is
// exactly when it matters — "Grafana's annotation calls are erroring" and "the OTLP span
// endpoint is erroring" are different incidents. Recording it after the query succeeded
// left every 500 unattributed.
//
// Closing the store is what makes the query fail without reaching into the handler:
// pgxpool.Close is idempotent, so newServer's own t.Cleanup(s.Close) still runs safely.
func TestFailedRenderStillRecordsTheAdapter(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	// Warmed before spans are recorded and the pool closes: the authenticator shares the
	// store's own pool, so an unwarmed cache would answer 503 from the credential lookup
	// instead of from the render this test means to exercise, and its own span would
	// pollute what this test asserts about the render's.
	warmCredentialCache(t, h, key, tn)

	rec, restore := recordSpans(t)
	defer restore()
	s.Close()

	resp := get(t, h, key, "/adapters/grafana/annotations?tenant="+tn+
		"&from=1789376400000&to=1789383600000")
	require.Equal(t, http.StatusInternalServerError, resp.Code,
		"a closed pool must fail the query; a 200 here means the test proved nothing")

	assert.Contains(t, spanAttrValues(rec, "", "horopter.adapter"), "grafana",
		"a failed render must still say which adapter asked")
}

// readRoute is one request against one egress read route, naming the pattern Routes
// registers it under so TestEveryReadRouteHasAFailureCase can match the two up.
type readRoute struct {
	pattern, url string
}

// readRoutes returns one well-formed request per egress read route, naming tenant tn.
// Every request is valid, so a failure can only come from the store.
func readRoutes(tn string) []readRoute {
	const rfc3339Window = "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	return []readRoute{
		{"GET /v1/changes", "/v1/changes?tenant=" + tn + rfc3339Window},
		{"GET /v1/events", "/v1/events?tenant=" + tn},
		{"GET /adapters/grafana/annotations",
			"/adapters/grafana/annotations?tenant=" + tn + "&from=1789376400000&to=1789383600000"},
		{"GET /adapters/otlp/spans", "/adapters/otlp/spans?tenant=" + tn + rfc3339Window},
	}
}

func (rr readRoute) serve(t *testing.T, h http.Handler, key string) *httptest.ResponseRecorder {
	t.Helper()
	return get(t, h, key, rr.url)
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
	key := readerKey(t, s, tn)
	// Warmed before the pool closes: see TestFailedRenderStillRecordsTheAdapter.
	warmCredentialCache(t, h, key, tn)
	s.Close()

	for _, route := range readRoutes(tn) {
		t.Run(route.pattern, func(t *testing.T) {
			records := captureLogs(t)
			rec := route.serve(t, h, key)
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
// user and name — so relaying it published that detail to anyone holding a valid
// credential. The wrapped error also carried the caller's own tenant string, making every
// failing route a reflector for caller-chosen text. Ingress's appendFailed withholds the
// same text for the same reason.
func TestFailedReadDoesNotRelayStoreErrorText(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	// Warmed before the pool closes: see TestFailedRenderStillRecordsTheAdapter.
	warmCredentialCache(t, h, key, tn)
	s.Close()

	for _, route := range readRoutes(tn) {
		t.Run(route.pattern, func(t *testing.T) {
			rec := route.serve(t, h, key)
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
		ID: id, Tenant: tn, IdempotencyKey: id, SchemaVersion: core.CurrentSchemaVersion,
		Source: "ci", Type: typ, Subject: subject, Actor: "ci-bot",
		OccurredAt: at, ReceivedAt: at, CorrelationRef: core.RefFromRow(ref),
		Attributes: map[string]any{}, Origin: core.OriginProcess,
		Locus: core.LocusFromRow("ci"),
	})
	require.NoError(t, err)
}

// refsIn returns the refs of a /v1/changes response, for comparing result sets.
func refsIn(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	changes := changesBody(t, rec)
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
	key := readerKey(t, s, tn)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	seedWith(t, s, tn, "ci/dep-90", "deployment.started", "acme/prod", at)
	seedWith(t, s, tn, "ci/inc-90", "incident.declared", "acme/staging", at)

	window := "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	assert.Equal(t, []string{"ci/dep-90", "ci/inc-90"},
		refsIn(t, get(t, h, key, "/v1/changes?tenant="+tn+window)),
		"precondition: both changes are in the window")

	assert.Equal(t, []string{"ci/dep-90"},
		refsIn(t, get(t, h, key, "/v1/changes?tenant="+tn+window+"&subject=acme/prod")),
		"subject= must filter on subject, not on type")
	assert.Equal(t, []string{"ci/inc-90"},
		refsIn(t, get(t, h, key, "/v1/changes?tenant="+tn+window+"&type=incident.declared")),
		"type= must filter on type, not on subject")
}

// status= is wired separately and is dropped just as silently: without it the endpoint
// answers every status query with the unfiltered set, so a dashboard asking for failures
// is handed everything and shows no error.
func TestQueryChangesFiltersByStatus(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	seedWith(t, s, tn, "ci/dep-91", "deployment.started", "acme/prod", at)
	seedWith(t, s, tn, "ci/dep-92", "deployment.started", "acme/prod", at)
	seedWith(t, s, tn, "ci/dep-92", "deployment.completed", "acme/prod", at.Add(time.Minute))

	window := "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	assert.Equal(t, []string{"ci/dep-91", "ci/dep-92"},
		refsIn(t, get(t, h, key, "/v1/changes?tenant="+tn+window)),
		"precondition: both changes are in the window")

	assert.Equal(t, []string{"ci/dep-92"},
		refsIn(t, get(t, h, key, "/v1/changes?tenant="+tn+window+"&status=completed")),
		"status= must narrow the result set")
	assert.Equal(t, []string{"ci/dep-91"},
		refsIn(t, get(t, h, key, "/v1/changes?tenant="+tn+window+"&status=in_progress")),
		"the complementary status must select the other change")
}

// seedSpec is one change to seed, named rather than positional. seedWith hardcodes source
// "ci" and empty attributes, which is enough for the subject and type filters and not for
// these.
//
// The stored ref's first segment is what change.locus is generated from, so ref "gha/dep-1"
// seeds a change in the gha locus.
//
// Named seedSpec, not seed: seed is already the package's existing single-event helper
// above, and a struct sharing its name would not compile alongside it.
type seedSpec struct {
	tenant, ref, source string
	attrs               map[string]any
}

func seedChange(t *testing.T, s *postgres.Store, sd seedSpec, at time.Time) {
	t.Helper()
	id := core.NewULID(time.Now(), rand.Reader)
	if sd.attrs == nil {
		sd.attrs = map[string]any{}
	}
	ref := core.RefFromRow(sd.ref)
	_, _, err := s.AppendEvent(context.Background(), core.Event{
		ID: id, Tenant: sd.tenant, IdempotencyKey: id,
		SchemaVersion: core.CurrentSchemaVersion,
		Source:        sd.source, Type: "deployment.started", Subject: "acme/prod",
		Actor: "ci-bot", OccurredAt: at, ReceivedAt: at,
		CorrelationRef: ref, Attributes: sd.attrs,
		Origin: core.OriginProcess, Locus: ref.Locus(),
	})
	require.NoError(t, err)
}

// TestQueryChangesFiltersByLocusAndSource pins the two scalar filters together, because
// they are wired in adjacent lines from adjacent query parameters and a transposition
// between them is invisible to every other test.
//
// The two changes share a source and differ in locus, then share a locus and differ in
// source, so each filter is the only thing that can select exactly one row — a clause
// reading the other column matches none or both, never the one expected.
func TestQueryChangesFiltersByLocusAndSource(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	seedChange(t, s, seedSpec{tenant: tn, ref: "ci/dep-1", source: "github"}, at)
	seedChange(t, s, seedSpec{tenant: tn, ref: "gha/dep-2", source: "github"}, at)
	seedChange(t, s, seedSpec{tenant: tn, ref: "gha/dep-3", source: "buildkite"}, at)

	window := "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	base := "/v1/changes?tenant=" + tn + window

	assert.Equal(t, []string{"ci/dep-1", "gha/dep-2", "gha/dep-3"},
		refsIn(t, get(t, h, key, base)), "precondition: all three are in the window")

	assert.Equal(t, []string{"ci/dep-1"}, refsIn(t, get(t, h, key, base+"&locus=ci")),
		"locus= must filter on the generated locus column, not on source")
	assert.Equal(t, []string{"gha/dep-3"},
		refsIn(t, get(t, h, key, base+"&source=buildkite")),
		"source= must filter on source, not on locus")
	assert.Equal(t, []string{"gha/dep-2"},
		refsIn(t, get(t, h, key, base+"&locus=gha&source=github")),
		"the two filters must compose rather than replace one another")

	assert.Equal(t, []string{"ci/dep-1", "gha/dep-2", "gha/dep-3"},
		refsIn(t, get(t, h, key, base+"&locus=&source=")),
		"a present but empty value is no filter, as an empty subject= is")
}

// TestQueryChangesFiltersByAllowlistedAttribute covers the ?attr.<key>= surface, including
// that everything after the prefix is the key: attribute keys are dotted by convention, so
// a parser splitting on "." would truncate every real one to "deployment".
func TestQueryChangesFiltersByAllowlistedAttribute(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	seedChange(t, s, seedSpec{tenant: tn, ref: "ci/dep-prod", source: "github",
		attrs: map[string]any{"deployment.environment.name": "prod"}}, at)
	seedChange(t, s, seedSpec{tenant: tn, ref: "ci/dep-stg", source: "github",
		attrs: map[string]any{"deployment.environment.name": "staging"}}, at)

	window := "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	base := "/v1/changes?tenant=" + tn + window

	assert.Equal(t, []string{"ci/dep-prod"},
		refsIn(t, get(t, h, key,
			base+"&attr.deployment.environment.name=prod")))

	assert.Equal(t, []string{"ci/dep-prod", "ci/dep-stg"},
		refsIn(t, get(t, h, key, base+"&attr.deployment.environment.name=")),
		"an empty value is no filter, as an empty subject= is")
}

// TestQueryChangesRefusesAnUnfilterableAttributeKey is the assertion that proves the
// allowlist reaches the HTTP surface. Filtering on an allowlisted key passes whether or not
// the allowlist is enforced; only a refusal discriminates.
//
// 400 rather than 500: an unknown key is the caller's mistake, and readFailed's 500 would
// tell them the server is broken when their query is. And 400 rather than an unfiltered
// 200, because a caller handed every row in answer to a narrowing question has been told
// something false.
//
// The empty-value case is the interesting one, and it is deliberately NOT treated as "no
// filter" the way an empty subject= is. What is wrong is the KEY, not the filter: accepting
// ?attr.typo.key= and then refusing ?attr.typo.key=prod would make one mistake behave two
// ways depending on whether the caller filled the box in, which is a worse answer than
// either. An allowlisted key with an empty value IS no filter — that is
// TestQueryChangesFiltersByAllowlistedAttribute's second assertion.
func TestQueryChangesRefusesAnUnfilterableAttributeKey(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	window := "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	base := "/v1/changes?tenant=" + tn + window

	for name, url := range map[string]string{
		"unknown key":              base + "&attr.service.version=2.5.0",
		"empty key":                base + "&attr.=prod",
		"unknown key, empty value": base + "&attr.service.version=",
	} {
		t.Run(name, func(t *testing.T) {
			rec := get(t, h, key, url)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "deployment.environment.name",
				"the refusal must list what IS filterable, or the caller has to guess")
		})
	}
}

// TestChangesRenderTheirSources pins the field to the response. A filter on a value the
// caller cannot read back is half a feature: source was asked for both returned AND
// filtered.
func TestChangesRenderTheirSources(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	seedChange(t, s, seedSpec{tenant: tn, ref: "ci/dep-1", source: "github"}, at)

	rec := get(t, h, key, "/v1/changes?tenant="+tn+
		"&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	changes := changesBody(t, rec)
	require.Len(t, changes, 1)
	assert.Equal(t, []any{"github"}, changes[0]["sources"])
	assert.NotContains(t, changes[0], "locus",
		"ref already carries the locus; a second copy is a second thing to keep in agreement")
}

// TestEveryNonProbeEgressRouteRequiresACredential asserts, by enumerating the mux rather
// than driving a hand-written list of URLs, that a credential guards every route but the
// two probes. A hand-written list of routes can go stale the moment a route is added; this
// cannot, because it is the same enumeration TestNoWriteRouteIsRegisteredAnywhere reads.
//
// isProbe itself is a hand-written two-entry list, so the count below is what keeps it
// honest: without it, a fifth non-probe route would just be looped over and asserted like
// the other four, and nothing would say the count had changed at all.
func TestEveryNonProbeEgressRouteRequiresACredential(t *testing.T) {
	srv, _, _ := newServerWith(t, 0, egress.DefaultLimits())
	h := srv.Routes()

	var nonProbe []string
	for _, route := range routesOf(t, h) { // enumerates the mux, never a hand-written list
		if !isProbe(route) {
			nonProbe = append(nonProbe, route)
		}
	}
	require.Len(t, nonProbe, 4,
		"egress has four non-probe routes; a fifth (or a third) must be a deliberate "+
			"change to this count, not a silent one")

	for _, route := range nonProbe {
		t.Run(route, func(t *testing.T) {
			assert.Equal(t, http.StatusUnauthorized, getUnauthenticated(t, h, route).Code)
		})
	}
}

// TestAuthRejectsBeforeAdmissionSpendsAPermit is the ordering guarantee Routes' shape
// exists for: s.authz(auth.CapRead, s.admit(...)), never the reverse. If admission ran
// first, an unauthenticated request would consume a read permit before being rejected — a
// free denial of service against the limiter — and the credential lookup itself takes a
// pool connection admission is sized from, so an unauthenticated flood would also compete
// for what admission exists to protect.
func TestAuthRejectsBeforeAdmissionSpendsAPermit(t *testing.T) {
	srv, s, tn := newServerWith(t, 1, testLimits())
	h := srv.Routes()
	key := readerKey(t, s, tn)
	seed(t, s, tn, "ci/dep-permit", "deployment.started", fixed)
	url := "/v1/changes?tenant=" + tn + "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"

	release := holdRead(t, h, key, url)
	defer release()
	require.Equal(t, 1, admittedCount(t, srv),
		"the limiter's one permit must be held, or this test proves nothing")

	// A wrong ordering — admission reached before auth — would not answer 401 here: the
	// limiter's one permit is held, so the concurrency gate alone would wait out
	// AdmissionWait and shed with 503, and admit's own tenant resolution (which needs the
	// principal auth attaches) would refuse with 400 before it ever found one to read.
	// Comparing InFlight() before and after this call does not discriminate a wrong
	// ordering from the correct one: a permit taken and released within this one
	// synchronous request is gone again by the time the caller reads the count afterward,
	// so that comparison would pass either way. 401 is the only outcome the correct
	// ordering produces.
	rec := getUnauthenticated(t, h, "GET /v1/changes")
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"auth outside admit: an unauthenticated request must be rejected before it can "+
			"reach a saturated limiter, not shed by it")
}

func TestATenantScopedCredentialCannotSelectAnotherTenant(t *testing.T) {
	srv, s, tn := newServerWith(t, 0, egress.DefaultLimits())
	h := srv.Routes()
	key := readerKey(t, s, tn)
	window := "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	assert.Equal(t, http.StatusForbidden,
		get(t, h, key, "/v1/changes?tenant=tnt_other"+window).Code)
}

func TestATenantScopedCredentialMaySelectItsOwnTenant(t *testing.T) {
	srv, s, tn := newServerWith(t, 0, egress.DefaultLimits())
	h := srv.Routes()
	key := readerKey(t, s, tn)
	window := "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	assert.Equal(t, http.StatusOK,
		get(t, h, key, "/v1/changes?tenant="+tn+window).Code,
		"naming your own tenant is redundant, not forbidden")
}

func TestASuperuserSelectsATenantByQueryParameter(t *testing.T) {
	srv, s, tn := newServerWith(t, 0, egress.DefaultLimits())
	h := srv.Routes()
	key := seedCredential(t, s, "superuser", "")
	window := "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	assert.Equal(t, http.StatusOK, get(t, h, key, "/v1/changes?tenant="+tn+window).Code)
}

func TestASuperuserNamingNoTenantIsRejected(t *testing.T) {
	srv, s, _ := newServerWith(t, 0, egress.DefaultLimits())
	h := srv.Routes()
	key := seedCredential(t, s, "superuser", "")
	rec := get(t, h, key, "/v1/changes?from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z")
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"a global credential has no tenant to default to; DefaultTenant is gone")
}

func TestASuperuserNamingAnUnknownTenantGets404(t *testing.T) {
	srv, s, _ := newServerWith(t, 0, egress.DefaultLimits())
	h := srv.Routes()
	key := seedCredential(t, s, "superuser", "")
	rec := get(t, h, key,
		"/v1/changes?tenant=tnt_doesnotexist&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "tnt_", "a 404 must not confirm which ids exist")
}

func TestFetchByRefNeedsNoWindow(t *testing.T) {
	srv, s, tn := newServerWith(t, 0, egress.DefaultLimits())
	h := srv.Routes()
	key := readerKey(t, s, tn)
	seed(t, s, tn, "github/deploy-1", "deployment.started", fixed)

	rec := get(t, h, key, "/v1/changes?ref="+url.QueryEscape("github/deploy-1"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, changesOf(t, rec), 1)
}

// TestFetchByRefRoundTripsADotDotSegment pins that ref is a query parameter, not a path
// segment: net/http's ServeMux path-cleans a URL path before it ever reaches a handler,
// which would silently collapse "legacy/../etc" into "legacy" if ref were routed that way.
func TestFetchByRefRoundTripsADotDotSegment(t *testing.T) {
	srv, s, tn := newServerWith(t, 0, egress.DefaultLimits())
	h := srv.Routes()
	key := readerKey(t, s, tn)
	seed(t, s, tn, "legacy/../etc", "deployment.started", fixed)

	rec := get(t, h, key, "/v1/changes?ref="+url.QueryEscape("legacy/../etc"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "legacy/../etc", changesOf(t, rec)[0].Ref,
		"a query parameter is not path-cleaned; this is why the lookup is not a path route")
}

// TestTheAnnotationPOSTRoutesAreGone drives both former POST routes. The two status codes
// differ because one pattern survives (as a GET) and the other does not: ServeMux answers
// 405 for a method mismatch on a registered path, 404 when nothing matches.
func TestTheAnnotationPOSTRoutesAreGone(t *testing.T) {
	srv, s, tn := newServerWith(t, 0, egress.DefaultLimits())
	h := srv.Routes()
	key := readerKey(t, s, tn)
	assert.Equal(t, http.StatusMethodNotAllowed,
		postTo(t, h, key, "/adapters/grafana/annotations").Code)
	assert.Equal(t, http.StatusNotFound,
		postTo(t, h, key, "/adapters/grafana/acme/annotations").Code,
		"no {tenant} pattern survives, so this path matches nothing at all")
}

// TestGrafanaAnnotationsRejectsARef pins the route's shape. The ref filter belongs to
// GET /v1/changes: it takes the full stored <locus>/<ref>, which no Grafana dashboard
// variable can produce, and it makes the window meaningless — this route answered "what
// overlapped from/to" while demanding a window changeSelection then discarded. Refusing it
// names the route that does the lookup instead of quietly offering a second spelling.
func TestGrafanaAnnotationsRejectsARef(t *testing.T) {
	srv, s, tn := newServerWith(t, 0, egress.DefaultLimits())
	h := srv.Routes()
	key := readerKey(t, s, tn)
	seed(t, s, tn, "github/deploy-1", "deployment.started", fixed)

	rec := get(t, h, key, "/adapters/grafana/annotations?tenant="+tn+
		"&from=1789376400000&to=1789383600000&ref="+url.QueryEscape("github/deploy-1"))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "/v1/changes?ref=",
		"the refusal must name the route that does support a ref lookup")
}

// TestListEventsReturnsOriginAndEdited pins the two server-set columns on the audit route.
// Both are stored and neither was readable: origin is the only record of whether a human or
// a process appended the event, which is exactly what an export exists to carry.
func TestListEventsReturnsOriginAndEdited(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	seed(t, s, tn, "ci/dep-origin", "deployment.started", fixed)

	rec := get(t, h, key, "/v1/events?tenant="+tn+"&ref="+url.QueryEscape("ci/dep-origin"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	events := eventsBody(t, rec)
	require.Len(t, events, 1)
	assert.Equal(t, string(core.OriginProcess), events[0]["origin"],
		"every Tier 0 credential is a machine key, so every event is origin=process")
	assert.Equal(t, false, events[0]["edited"])
}

// TestListEventsReturnsLifecycle pins the producer-set field the whole opening-a-change
// branch exists for: a producer auditing its own log must be able to tell an explicit
// "lifecycle": "started" from a ".started" type suffix, and GET /v1/events was silently
// omitting the one field that distinguishes them.
func TestListEventsReturnsLifecycle(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	id := core.NewULID(time.Now(), rand.Reader)
	_, _, err := s.AppendEvent(context.Background(), core.Event{
		ID: id, Tenant: tn, IdempotencyKey: id, SchemaVersion: core.CurrentSchemaVersion,
		Source: "ci", Type: "deployment.initiated", Subject: "acme/prod", Actor: "ci-bot",
		OccurredAt: fixed, ReceivedAt: fixed, CorrelationRef: core.RefFromRow("ci/dep-lifecycle"),
		Lifecycle: core.LifecycleStarted, Origin: core.OriginProcess,
		Locus: core.LocusFromRow("ci"),
	})
	require.NoError(t, err)

	rec := get(t, h, key, "/v1/events?tenant="+tn+"&ref="+url.QueryEscape("ci/dep-lifecycle"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	events := eventsBody(t, rec)
	require.Len(t, events, 1)
	assert.Equal(t, string(core.LifecycleStarted), events[0]["lifecycle"],
		"the explicit assertion must render, not just the type suffix that would imply it")
}

// TestListEventsReturnsLocus pins the field that explains the ref transformation this route
// otherwise shows without its input: the producer sent "dep-locus" and the stored
// correlation_ref reads back "gha/dep-locus". Without locus, nothing in the rendered event
// says where the prefix came from.
//
// The locus deliberately differs from the source, because they default to the same value and
// a fixture using "ci" for both would pass while rendering either one.
func TestListEventsReturnsLocus(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	id := core.NewULID(time.Now(), rand.Reader)
	_, _, err := s.AppendEvent(context.Background(), core.Event{
		ID: id, Tenant: tn, IdempotencyKey: id, SchemaVersion: core.CurrentSchemaVersion,
		Source: "ci", Type: "deployment.started", Subject: "acme/prod", Actor: "ci-bot",
		OccurredAt: fixed, ReceivedAt: fixed, CorrelationRef: core.RefFromRow("gha/dep-locus"),
		Origin: core.OriginProcess, Locus: core.LocusFromRow("gha"),
	})
	require.NoError(t, err)

	rec := get(t, h, key, "/v1/events?tenant="+tn+"&ref="+url.QueryEscape("gha/dep-locus"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	events := eventsBody(t, rec)
	require.Len(t, events, 1)
	assert.Equal(t, "gha", events[0]["locus"],
		"the locus every stored ref is prefixed with must be readable back")
	assert.Equal(t, "ci", events[0]["source"],
		"and must not be confused with source, which defaults to the same value")
}

func requestLines(records []map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if r["msg"] == "request" {
			out = append(out, r)
		}
	}
	return out
}

func TestAReadLogsItsQueryTenantAndCredential(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	records := captureLogs(t)
	get(t, h, key, "/v1/changes?ref=ci/dep-1")
	prefix, _, ok := auth.SplitKey(key)
	require.True(t, ok)
	lines := requestLines(records())
	require.Len(t, lines, 1)
	assert.Equal(t, "GET /v1/changes", lines[0]["route"])
	assert.Equal(t, "ref=ci/dep-1", lines[0]["query"])
	assert.Equal(t, tn, lines[0]["tenant"])
	assert.Equal(t, prefix, lines[0]["credential"])
}

func TestAnUnauthenticatedReadLogsNoQuery(t *testing.T) {
	h, _, _ := newServer(t)
	records := captureLogs(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/changes?ref=chosen-by-anyone", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	lines := requestLines(records())
	require.Len(t, lines, 1)
	assert.NotContains(t, lines[0], "query")
	assert.NotContains(t, lines[0], "credential")
}

func TestALongQueryIsCutAndFlagged(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	records := captureLogs(t)
	get(t, h, key, "/v1/changes?ref=ci/"+strings.Repeat("q", 4<<10))
	line := requestLines(records())[0]
	assert.Len(t, line["query"], 1024)
	assert.Equal(t, true, line["query_truncated"])
}

// changesBody decodes a /v1/changes envelope's rows.
func changesBody(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var body struct {
		Changes []map[string]any `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.NotNil(t, body.Changes, "the envelope always carries changes, [] when none")
	return body.Changes
}

// eventsBody decodes a /v1/events envelope's rows.
func eventsBody(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var body struct {
		Events []map[string]any `json:"events"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.NotNil(t, body.Events, "the envelope always carries events, [] when none")
	return body.Events
}

func TestBothNeutralRoutesAnswerWithAnEnvelope(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	seed(t, s, tn, "ci/env-1", "deployment.started", time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC))
	changes := changesBody(t, get(t, h, key,
		"/v1/changes?from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"))
	assert.Len(t, changes, 1)
	events := eventsBody(t, get(t, h, key, "/v1/events?ref=ci/env-1"))
	assert.Len(t, events, 1)
	empty := changesBody(t, get(t, h, key,
		"/v1/changes?from=2020-01-01T00:00:00Z&to=2020-01-02T00:00:00Z"))
	assert.Empty(t, empty)
}

// follow reads every page of url by following next, returning all rows and the page count.
func follow(t *testing.T, h http.Handler, key, url, field string) ([]map[string]any, int) {
	t.Helper()
	var rows []map[string]any
	pages := 0
	for url != "" {
		rec := get(t, h, key, url)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		var page []map[string]any
		require.NoError(t, json.Unmarshal(body[field], &page))
		rows = append(rows, page...)
		pages++
		url = ""
		if raw, ok := body["next"]; ok {
			require.NoError(t, json.Unmarshal(raw, &url))
			assert.Equal(t, "<"+url+">; rel=\"next\"", rec.Header().Get("Link"))
			assert.Equal(t, "true", rec.Header().Get(egress.TruncatedHeader),
				"a page with a successor is still a cut result for callers that do not page")
			require.NotEmpty(t, page, "next is sent only with at least one row")
		} else {
			assert.Empty(t, rec.Header().Get("Link"))
		}
	}
	return rows, pages
}

func TestFollowingNextReturnsEveryChangeOnceInOrder(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	// seedManyChanges seeds every change at the same instant, so every page boundary falls
	// inside a run of equal start_at values: only the ref tie-break keeps them apart.
	seedManyChanges(t, s, tn, 2500)
	rows, pages := follow(t, h, key,
		"/v1/changes?from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z", "changes")
	assert.Equal(t, 3, pages)
	assert.Len(t, rows, 2500)
	// Uniqueness and the count, not Go string order: Postgres orders ref by the database's
	// collation, which need not agree with byte order, and the keyset compares the same way.
	seen := map[string]bool{}
	for _, row := range rows {
		ref, _ := row["ref"].(string)
		assert.False(t, seen[ref], "ref %s returned twice", ref)
		seen[ref] = true
	}
}

func TestFollowingNextReturnsEveryEventOnce(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	seedManyEvents(t, s, tn, 1200)
	rows, pages := follow(t, h, key, "/v1/events?limit=500", "events")
	assert.Equal(t, 3, pages)
	assert.Len(t, rows, 1200)
}

func TestACursorForADifferentQueryIs400(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	seedManyChanges(t, s, tn, 3)
	first := get(t, h, key, "/v1/changes?from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z&limit=1")
	var body struct {
		Next string `json:"next"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &body))
	require.NotEmpty(t, body.Next)
	u, err := url.Parse(body.Next)
	require.NoError(t, err)
	q := u.Query()
	q.Set("status", "completed")
	rec := get(t, h, key, u.Path+"?"+q.Encode())
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "cursor was issued for a different query")

	garbage := get(t, h, key,
		"/v1/changes?from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z&cursor=%21%21")
	assert.Equal(t, http.StatusBadRequest, garbage.Code)
}

func TestTheAdapterRoutesIgnoreACursor(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	seedManyChanges(t, s, tn, 3)
	plain := get(t, h, key, "/adapters/otlp/spans?from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z")
	withCursor := get(t, h, key,
		"/adapters/otlp/spans?from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z&cursor=anything")
	assert.Equal(t, plain.Code, withCursor.Code)
	assert.Equal(t, plain.Body.String(), withCursor.Body.String())
	assert.Empty(t, withCursor.Header().Get("Link"))
}

// seedChangesWithSubject seeds n correlated changes under subject, an hour apart.
func seedChangesWithSubject(t *testing.T, s *postgres.Store, tn, subject string, n int) {
	t.Helper()
	for i := range n {
		id := core.NewULID(time.Now(), rand.Reader)
		at := time.Date(2026, 9, 14, 9, 10+i, 0, 0, time.UTC)
		_, _, err := s.AppendEvent(context.Background(), core.Event{
			ID: id, Tenant: tn, IdempotencyKey: id, SchemaVersion: core.CurrentSchemaVersion,
			Source: "ci", Type: "deployment.started", Subject: subject,
			OccurredAt: at, ReceivedAt: at,
			CorrelationRef: core.RefFromRow(fmt.Sprintf("ci/esc-%d", i)),
			Origin:         core.OriginProcess, Locus: core.LocusFromRow("ci"),
		})
		require.NoError(t, err)
	}
}

func TestNextKeepsAFilterThatNeedsEscaping(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	seedChangesWithSubject(t, s, tn, "a&b/c", 3)
	rows, pages := follow(t, h, key, "/v1/changes?from=2026-09-14T09:00:00Z"+
		"&to=2026-09-14T11:00:00Z&subject=a%26b%2Fc&limit=1", "changes")
	assert.Equal(t, 3, pages)
	assert.Len(t, rows, 3)
}

// TestFollowingNextAcrossSubSecondStartsReturnsEachChangeOnce: real starts carry fractions of a
// second, and the cursor must keep them. A cursor cut back to the whole second resumes before
// rows it already returned, so a caller paging with curl sees them again, page after page.
func TestFollowingNextAcrossSubSecondStartsReturnsEachChangeOnce(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	base := time.Date(2026, 9, 14, 9, 10, 0, 0, time.UTC)
	for i := range 5 {
		id := core.NewULID(time.Now(), rand.Reader)
		at := base.Add(time.Duration(i+1) * 150 * time.Millisecond)
		_, _, err := s.AppendEvent(context.Background(), core.Event{
			ID: id, Tenant: tn, IdempotencyKey: id, SchemaVersion: core.CurrentSchemaVersion,
			Source: "ci", Type: "deployment.started", Subject: "acme/prod",
			OccurredAt: at, ReceivedAt: at,
			CorrelationRef: core.RefFromRow(fmt.Sprintf("ci/sub-%d", i)),
			Origin:         core.OriginProcess, Locus: core.LocusFromRow("ci"),
		})
		require.NoError(t, err)
	}
	seen := map[string]int{}
	url := "/v1/changes?from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z&limit=2"
	for pages := 0; url != ""; pages++ {
		require.Less(t, pages, 10, "paging must end; refs so far %v", seen)
		rec := get(t, h, key, url)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		for _, row := range changesBody(t, rec) {
			ref, _ := row["ref"].(string)
			seen[ref]++
		}
		var body struct {
			Next string `json:"next"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		url = body.Next
	}
	assert.Len(t, seen, 5)
	for ref, n := range seen {
		assert.Equal(t, 1, n, "%s returned %d times", ref, n)
	}
}

func TestSubjectPatternsFilterEveryChangeRoute(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	seedChange(t, s, seedSpec{tenant: tn, ref: "ci/dep-1", source: "github"}, at)

	window := "&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z"
	assert.Equal(t, []string{"ci/dep-1"},
		refsIn(t, get(t, h, key, "/v1/changes?tenant="+tn+window+"&subject=acme/*")))
	assert.Empty(t, refsIn(t, get(t, h, key, "/v1/changes?tenant="+tn+window+"&subject=other/*")))

	for _, route := range []string{
		"/v1/changes?tenant=" + tn + window,
		"/adapters/grafana/annotations?from=1789376400000&to=1789383600000",
		"/adapters/otlp/spans?from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z",
	} {
		rec := get(t, h, key, route+"&subject=%5B")
		assert.Equal(t, http.StatusBadRequest, rec.Code, route)
		assert.Contains(t, rec.Body.String(), "bad subject pattern", route)
	}
}
