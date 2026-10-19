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

// The shared database is never truncated, so ids are unique per process and per call.
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

// The concurrency limit starts at the pool size, so a small pool lets one held read fill it.
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

// Cleanup uses testdb's pool: several tests close s's pool before the test ends.
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

func readerKey(t *testing.T, s *postgres.Store, tn string) string {
	t.Helper()
	return seedCredential(t, s, "reader", tn)
}

// Keep in this file: expansion_test.go is excluded under -race.
func warmCredentialCache(t *testing.T, h http.Handler, key, tn string) {
	t.Helper()
	rec := get(t, h, key, "/v1/changes?tenant="+tn+
		"&from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

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

func seedManyChanges(t *testing.T, s *postgres.Store, tn string, n int) {
	t.Helper()
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	batch := &pgx.Batch{}
	for range n {
		ref := fmt.Sprintf("ci/bulk-%s-%s-%d", t.Name(), procToken, tenantSeq.Add(1))
		// start_at and end_effective are generated columns and reject supplied values.
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

func get(t *testing.T, h http.Handler, key, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getUnauthenticated(t *testing.T, h http.Handler, route string) *httptest.ResponseRecorder {
	t.Helper()
	method, path, ok := strings.Cut(route, " ")
	require.True(t, ok, "route pattern %q must be \"METHOD /path\"", route)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func postTo(t *testing.T, h http.Handler, key, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ServeMux cannot list its routes, so they are read from server.go's AST.
func routesOf(t *testing.T, _ http.Handler) []string {
	t.Helper()
	return collectRoutePatterns(t)
}

func isProbe(route string) bool {
	return route == "GET /healthz" || route == "GET /readyz"
}

func admittedCount(t *testing.T, srv *egress.Server) int {
	t.Helper()
	return egress.InFlight(srv)
}

type jsonChange struct {
	Ref string `json:"ref"`
}

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

	// The "" ref seeded above is the suite's only point event.
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

// A negative or non-numeric limit must not fall through to the store's 0 = unlimited.
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

func TestReadyzReportsReadyWithLiveDatabase(t *testing.T) {
	h, _, _ := newServer(t)
	rec := getUnauthenticated(t, h, "GET /readyz")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Runs without a database: the only liveness coverage on a DB-less CI leg.
func TestHealthzWorksWithNilStore(t *testing.T) {
	authn := auth.New(nil, fixedClock, auth.DefaultOptions())
	srv, err := egress.New(nil, fixedClock, egress.DefaultLimits(), authn)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "healthz must not require a database")
}

func TestNewRejectsANilAuthenticator(t *testing.T) {
	srv, err := egress.New(nil, fixedClock, egress.DefaultLimits(), nil)
	require.Error(t, err, "a nil authenticator would panic on the first request rather "+
		"than reject it")
	assert.Nil(t, srv, "a rejected configuration must not yield a usable server")
}

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

func TestNoWriteRouteIsRegisteredAnywhere(t *testing.T) {
	patterns := collectRoutePatterns(t)

	require.NotEmpty(t, patterns, "Routes() must register at least one handler")
	for _, p := range patterns {
		assert.True(t, strings.HasPrefix(p, "GET "),
			"route pattern %q is not a GET route; egress must expose no write route", p)
	}
}

// Asking for exactly as many rows as exist is the case a count cannot tell from truncation.
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

// An absent started_at must stay absent, never default to first_seen_at.
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

	seed(t, s, tn, "ci/dep-facts", "deployment.annotated", seen)

	inferred := only()
	assert.Equal(t, seen.Format(time.RFC3339Nano), inferred["first_seen_at"],
		"the inference must be emitted, not just folded into start")
	assert.NotContains(t, inferred, "started_at",
		"no producer has asserted a start, and absent is the only honest rendering of that")
	assert.Equal(t, seen.Format(time.RFC3339Nano), inferred["start"],
		"start falls through to the inference while the assertion is NULL")

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

// pgxpool.Close is idempotent, so newServer's own cleanup close stays safe.
func TestReadyzReports503WhenTheDatabaseIsUnreachable(t *testing.T) {
	h, s, _ := newServer(t)
	s.Close()

	rec := getUnauthenticated(t, h, "GET /readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "database unreachable",
		"an operator reading the probe's body must be told what is wrong")
}

// pgx's error names the DB user, name, host and port; the probe is unauthenticated.
func TestReadyzDoesNotRelayTheDatabaseError(t *testing.T) {
	h, s, _ := newServer(t)
	s.Close()

	rec := getUnauthenticated(t, h, "GET /readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "closed pool",
		"the database error must not reach an unauthenticated caller")
}

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

// An inverted window returned the changes spanning it, not an empty list.
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

// int64-max millis overflowed pgx's timestamptz encoding and silently returned [].
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

func TestGrafanaAcceptsTheEpochBoundsThemselves(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
	url := fmt.Sprintf("/adapters/grafana/annotations?tenant=%s&from=0&to=%d",
		tn, core.OpenSentinel.UnixMilli())
	rec := get(t, h, key, url)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Copied from postgres/append_test.go: a _test package cannot be imported.
func recordSpans(t *testing.T) (*tracetest.SpanRecorder, func()) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	return rec, func() { otel.SetTracerProvider(prev) }
}

// Spans lacking key are skipped. Value.String, not Emit: staticcheck SA1019.
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

func TestFailedRenderStillRecordsTheAdapter(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
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

type readRoute struct {
	pattern, url string
}

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

// Swapping the process-wide default is safe only while no test here calls t.Parallel.
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

func TestEveryFailedReadIsLoggedWithItsCause(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
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

func TestFailedReadDoesNotRelayStoreErrorText(t *testing.T) {
	h, s, tn := newServer(t)
	key := readerKey(t, s, tn)
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

// The changes differ in subject AND type, so a transposed wire selects nothing.
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

// The ref's first segment becomes change.locus.
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

// Each filter alone selects exactly one row; a transposed column matches none or both.
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

// Attribute keys are dotted, so everything after the prefix is the key.
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

// An unknown key is refused even with an empty value: the key is the mistake.
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

// The count keeps isProbe's hand-written list honest.
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

	// Only 401 discriminates: an InFlight comparison passes for either ordering.
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

// ServeMux path-cleans paths, so ref must stay a query parameter.
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

// ServeMux answers 405 for a method mismatch, 404 when nothing matches.
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

// Locus differs from source on purpose: they default to the same value.
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

func changesBody(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var body struct {
		Changes []map[string]any `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	require.NotNil(t, body.Changes, "the envelope always carries changes, [] when none")
	return body.Changes
}

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
	// Every change starts at the same instant: only the ref tie-break separates pages.
	seedManyChanges(t, s, tn, 2500)
	rows, pages := follow(t, h, key,
		"/v1/changes?from=2026-09-14T09:00:00Z&to=2026-09-14T11:00:00Z", "changes")
	assert.Equal(t, 3, pages)
	assert.Len(t, rows, 2500)
	// Postgres collation, not Go byte order, decides ref order, so check uniqueness and count.
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

// A cursor cut to whole seconds re-returns rows on every page.
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
