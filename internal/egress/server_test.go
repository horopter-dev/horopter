package egress_test

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
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
// nothing in the brief's suite drove a malformed value through epochMillisParam, the
// parser the Grafana endpoint actually uses. The needle is "epoch milliseconds"
// rather than "from": Task 11 had an assertion on the bare word "id" satisfied by the
// word invalid in the error's own prefix, so this pins wording long enough that only
// the intended message can match it.
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
	srv := egress.New(nil, func() time.Time { return fixed })
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "healthz must not require a database")
}

// TestRoutesRegisterOnlyGETHandlers asserts, with a general structural check, that
// egress exposes no write route anywhere: it parses server.go, walks every
// mux.Handle/mux.HandleFunc call inside Routes(), and asserts each pattern string
// starts with "GET ". A single HTTP probe against one path can only ever prove that
// one path has no write twin; a write route registered at any other path —
// POST /v1/ingest, PUT /v1/changes — would pass every HTTP-level test in this file
// while never being exercised. Reading the source is what generalizes the property to
// "no write route anywhere", not just "no write route at this one URL". It needs no
// database and runs unconditionally, like TestHealthzWorksWithNilStore.
func TestRoutesRegisterOnlyGETHandlers(t *testing.T) {
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
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc" {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || recv.Name != "mux" {
				return true
			}
			require.NotEmpty(t, call.Args, "mux.%s call with no arguments", sel.Sel.Name)
			lit, ok := call.Args[0].(*ast.BasicLit)
			require.True(t, ok && lit.Kind == token.STRING,
				"mux.%s's first argument must be a string literal pattern", sel.Sel.Name)
			pattern, err := strconv.Unquote(lit.Value)
			require.NoError(t, err)
			patterns = append(patterns, pattern)
			return true
		})
		return false
	})

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
