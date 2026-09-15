package ingress_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yo61/horopter/internal/ingress"
	"github.com/yo61/horopter/internal/store/postgres"
)

var fixed = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

const testToken = "test-token"

// The tests share a database that nothing truncates, so every tenant and idempotency
// key a test puts into it has to be one no earlier execution used — otherwise a POST
// that should create a row finds one already there and returns 200 where the test
// expects 201, failing for a reason unrelated to the code under test.
//
// procToken separates one `go test` process from the next; idSeq separates every call
// within a process, which is what makes -count=N do real work instead of colliding
// with its own prior iteration.
var (
	procToken = strconv.FormatInt(time.Now().UnixNano(), 36)
	idSeq     atomic.Uint64
)

// uniqueToken returns a token no other call in any execution has produced.
func uniqueToken() string {
	return procToken + "-" + strconv.FormatUint(idSeq.Add(1), 36)
}

// tenant returns a tenant id unique to this call, naming the test for traceability.
func tenant(t *testing.T) string {
	t.Helper()
	return "test-" + t.Name() + "-" + uniqueToken()
}

// idemKey returns an idempotency key unique to this call. suffix names the call for
// traceability and plays no part in uniqueness.
func idemKey(t *testing.T, suffix string) string {
	t.Helper()
	return uniqueToken() + "-" + t.Name() + "-" + suffix
}

func newServer(t *testing.T) (http.Handler, *postgres.Store) {
	t.Helper()
	v := os.Getenv("HOROPTER_TEST_DSN")
	if v == "" {
		t.Skip("set HOROPTER_TEST_DSN to run ingress tests (docker compose up -d db)")
	}
	s, err := postgres.Open(context.Background(), v)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	// entropy is crypto/rand.Reader, not a seeded math/rand source, and that is
	// deliberate: a seeded PRNG was tried here for determinism, but determinism is
	// exactly what caused a collision — the same seed plus the same fixed clock below
	// produces a byte-identical first ULID in every test, and event.id is a global
	// primary key, so the second test to insert one fails on a unique-constraint
	// violation. Nothing in this suite asserts on a generated event id (only on refs,
	// status codes and response bodies), so the determinism bought nothing while
	// guaranteeing a collision. crypto/rand.Reader is also what cmd/horopter-ingress
	// uses in production, so the tests exercise the real entropy source.
	srv, err := ingress.New(s, func() time.Time { return fixed }, rand.Reader, testToken)
	require.NoError(t, err)
	return srv.Routes(), s
}

func post(t *testing.T, h http.Handler, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPostEventCreatesChange(t *testing.T) {
	h, _ := newServer(t)
	body := fmt.Sprintf(`{
		"tenant":%q,"source":"buildkite","type":"deployment.started",
		"subject":"acme/prod","actor":"ci-bot","occurred_at":"2026-09-14T10:00:00Z",
		"correlation_ref":"dep-7","idempotency_key":%q,
		"attributes":{"service.version":"2.5.0"}}`, tenant(t), idemKey(t, "1"))
	rec := post(t, h, "/v1/events", body, testToken)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"ref":"dep-7"`)
}

func TestPostDuplicateIdempotencyKeyReturns200(t *testing.T) {
	h, _ := newServer(t)
	body := fmt.Sprintf(`{"tenant":%q,"source":"ci","type":"deployment.started",
		"subject":"acme/prod","occurred_at":"2026-09-14T10:00:00Z",
		"correlation_ref":"dep-7","idempotency_key":%q}`, tenant(t), idemKey(t, "dup"))
	rec1 := post(t, h, "/v1/events", body, testToken)
	require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
	assert.Equal(t, http.StatusOK, post(t, h, "/v1/events", body, testToken).Code,
		"a CI retry is normal traffic, not an error")
}

func TestPostInvalidEventReturns400WithReason(t *testing.T) {
	h, _ := newServer(t)
	body := fmt.Sprintf(`{"tenant":%q,"source":"ci","type":"deployment.started",
		  "occurred_at":"2026-09-14T10:00:00Z"}`, tenant(t))
	rec := post(t, h, "/v1/events", body, testToken)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "subject")
}

func TestIngressRequiresBearerToken(t *testing.T) {
	h, _ := newServer(t)
	body := fmt.Sprintf(`{"tenant":%q,"source":"ci","type":"deployment.started",
		"subject":"acme/prod","occurred_at":"2026-09-14T10:00:00Z"}`, tenant(t))
	assert.Equal(t, http.StatusUnauthorized, post(t, h, "/v1/events", body, "").Code)
	assert.Equal(t, http.StatusUnauthorized, post(t, h, "/v1/events", body, "wrong").Code)
}

func TestHealthzNeedsNoToken(t *testing.T) {
	h, _ := newServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "probes must not require credentials")
}

func TestCloudEventIngest(t *testing.T) {
	h, _ := newServer(t)
	body := fmt.Sprintf(`{"specversion":"1.0","id":%q,"source":"/buildkite/pipelines/api",
		"type":"deployment.started","subject":"acme/prod","time":"2026-09-14T10:00:00Z",
		"datacontenttype":"application/json",
		"data":{"tenant":%q,"correlation_ref":"dep-7","service.version":"2.5.0"}}`,
		idemKey(t, "ce"), tenant(t))
	req := httptest.NewRequest(http.MethodPost, "/v1/events/cloudevents",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/cloudevents+json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// TestHealthzWorksWithNilStore proves healthz never touches the database, without
// needing a live one: New never dereferences its store argument, and a Server built
// with a nil store still has to answer 200 from healthz for that to hold. It runs
// unconditionally — no HOROPTER_TEST_DSN skip — because it is the one property in this
// suite that a database-less CI leg can actually exercise: every other test here skips
// without a live Postgres, so without this one the liveness/readiness split would have
// zero coverage in that environment. If a future change reintroduces a
// Pool().Ping call into healthz, this test panics on the nil store and fails.
func TestHealthzWorksWithNilStore(t *testing.T) {
	srv, err := ingress.New(nil, func() time.Time { return fixed }, rand.Reader, testToken)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "healthz must not require a database")
}

// TestCloudEventMissingIDReturns400 exists because the CloudEvents id is half the
// idempotency key: an unvalidated envelope with no id still mints a NON-EMPTY key —
// idempotencyKey("/buildkite/pipelines/api", "") ends in "/" with nothing after it — and
// a non-empty key is deduped, because AppendEvent skips dedup only on "". So every
// id-less envelope from one source would collapse into whichever arrived first, and each
// later one would be answered 200 {"created": false}: distinct facts silently dropped
// from a system of record, with nothing surfaced and nothing for an operator to notice.
// The spec marks id REQUIRED; rejecting it here is what makes that failure impossible
// instead of latent.
func TestCloudEventMissingIDReturns400(t *testing.T) {
	h, _ := newServer(t)
	body := fmt.Sprintf(`{"specversion":"1.0","source":"/buildkite/pipelines/api",
		"type":"deployment.started","subject":"acme/prod","time":"2026-09-14T10:00:00Z",
		"datacontenttype":"application/json",
		"data":{"tenant":%q,"correlation_ref":"dep-7"}}`, tenant(t))
	req := httptest.NewRequest(http.MethodPost, "/v1/events/cloudevents",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/cloudevents+json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	// "id" alone would also match inside "invalid" (inval-id) regardless of whether the
	// missing-id detail is actually present in the body, so the assertion has to pin the
	// SDK's field-level wording, not just the word "id".
	assert.Contains(t, rec.Body.String(), "id: MUST",
		"the response must name the missing required field")
}

// TestCloudEventDuplicateIsIdempotent exists to give the "CloudEvents handler ignores
// the envelope's identity" mutation something to kill. source and id together form the
// idempotency key, so replaying the identical envelope — the shape a retried delivery
// takes — must return 200, not a second created row. Its sibling,
// TestCloudEventsFromDifferentSourcesSharingAnIDAreDistinct, covers the other half of
// that key; neither test alone pins both.
func TestCloudEventDuplicateIsIdempotent(t *testing.T) {
	h, _ := newServer(t)
	body := fmt.Sprintf(`{"specversion":"1.0","id":%q,"source":"/buildkite/pipelines/api",
		"type":"deployment.started","subject":"acme/prod","time":"2026-09-14T10:00:00Z",
		"datacontenttype":"application/json",
		"data":{"tenant":%q,"correlation_ref":"dep-7","service.version":"2.5.0"}}`,
		idemKey(t, "ce-dup"), tenant(t))
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/events/cloudevents",
			bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/cloudevents+json")
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	first := send()
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	second := send()
	assert.Equal(t, http.StatusOK, second.Code, second.Body.String(),
		"a redelivered CloudEvent must not create a second event")
}

// TestPostEventReturns503WhenTheStoreFails is the counterpart to
// TestPostInvalidEventReturns400WithReason, and the pair is the reason core.ErrInvalidEvent
// exists. Every AppendEvent failure used to be answered 400 with the raw error text.
// Producers treat 4xx as "do not retry", so a thirty-second database blip permanently lost
// every event posted during it, silently — and the raw text leaked connection strings,
// table names and constraint names to callers who are not operators.
//
// Closing the pool is a real transient store failure rather than a faked error: every
// subsequent AppendEvent fails inside pool.Begin, which is exactly where a database outage
// lands. pgxpool.Close is guarded by a sync.Once, so newServer's own t.Cleanup close is
// still safe.
func TestPostEventReturns503WhenTheStoreFails(t *testing.T) {
	h, s := newServer(t)
	s.Close()

	body := fmt.Sprintf(`{"tenant":%q,"source":"ci","type":"deployment.started",
		"subject":"acme/prod","occurred_at":"2026-09-14T10:00:00Z",
		"correlation_ref":"dep-7","idempotency_key":%q}`, tenant(t), idemKey(t, "down"))
	rec := post(t, h, "/v1/events", body, testToken)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "retried",
		"the body must tell the producer this is transient, which is the whole point of 503")
	assert.NotContains(t, rec.Body.String(), "closed pool",
		"the internal error text must not reach the caller")
	assert.NotContains(t, strings.ToLower(rec.Body.String()), "postgres",
		"the response must not name the store or leak its connection detail")
}

// TestCloudEventsFromDifferentSourcesSharingAnIDAreDistinct pins the composite
// idempotency key. CloudEvents guarantees uniqueness of source + id, not id alone, so a
// build number or a bare "1" is a conformant id — and two producers sharing one used to
// mean the second, genuinely distinct event was silently swallowed and answered
// 200 {"created": false}. A dropped fact reported as a successful retry is the worst shape
// that failure can take in a system of record.
func TestCloudEventsFromDifferentSourcesSharingAnIDAreDistinct(t *testing.T) {
	h, s := newServer(t)
	tn := tenant(t)
	sharedID := idemKey(t, "shared-across-sources")

	send := func(source string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"specversion":"1.0","id":%q,"source":%q,
			"type":"deployment.started","subject":"acme/prod","time":"2026-09-14T10:00:00Z",
			"datacontenttype":"application/json",
			"data":{"tenant":%q,"correlation_ref":"dep-7"}}`, sharedID, source, tn)
		req := httptest.NewRequest(http.MethodPost, "/v1/events/cloudevents",
			bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/cloudevents+json")
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	first := send("/buildkite/pipelines/api")
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	second := send("/github/actions/api")
	require.Equal(t, http.StatusCreated, second.Code, second.Body.String(),
		"two sources sharing a CloudEvents id are two distinct events; keying on the id "+
			"alone swallows the second and reports it as a retry")

	evs, _, err := s.ListEvents(context.Background(), postgres.EventQuery{Tenant: tn})
	require.NoError(t, err)
	assert.Len(t, evs, 2, "both events must be in the log, not one")
}

// TestNewRejectsAnEmptyToken pins the guard where it is structural rather than remembered.
// subtle.ConstantTimeCompare of two empty slices returns 1, so a Server built with an
// empty token authenticates a request carrying no Authorization header at all — the write
// side of a system of record, open to anyone who can reach the port. It used to be
// prevented only by a check in cmd/horopter-ingress, which any other caller (a test, a
// future embedding, a second binary) would bypass without noticing.
//
// It runs unconditionally: a constructor that refuses its argument needs no database.
func TestNewRejectsAnEmptyToken(t *testing.T) {
	srv, err := ingress.New(nil, func() time.Time { return fixed }, rand.Reader, "")
	require.Error(t, err,
		"an empty token makes every unauthenticated request authenticate successfully")
	assert.Nil(t, srv, "a rejected configuration must not yield a usable server")
	assert.Contains(t, err.Error(), "HOROPTER_INGRESS_TOKEN",
		"the error must name the setting an operator has to fix")
}

// TestPostEventRejectsAnOversizedBody covers the body cap on both readers. Neither reader
// had one: json.Decode and io.ReadAll both consume the whole body before anything is
// validated, so a single request decided how much the process allocated. 413 rather than
// 400 is the honest status — the body may be perfectly well formed and merely too large —
// and reporting it as malformed JSON would send the caller hunting for a syntax error
// that does not exist.
func TestPostEventRejectsAnOversizedBody(t *testing.T) {
	h, _ := newServer(t)
	huge := fmt.Sprintf(`{"tenant":%q,"source":"ci","type":"deployment.started",
		"subject":"acme/prod","occurred_at":"2026-09-14T10:00:00Z",
		"attributes":{"blob":%q}}`, tenant(t), strings.Repeat("x", ingress.MaxBodyBytes+1))

	for path, contentType := range map[string]string{
		"/v1/events":             "application/json",
		"/v1/events/cloudevents": "application/cloudevents+json",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(huge))
			req.Header.Set("Content-Type", contentType)
			req.Header.Set("Authorization", "Bearer "+testToken)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "exceeds",
				"the caller must be told the body was too large, not that it was malformed")
		})
	}
}

// unauthenticatedProbeRoutes is TestEveryNonProbeRouteRequiresTheBearerToken's
// documented exception list to "every route is registered through s.auth": the
// liveness and readiness probes. A probe that demanded a credential could not be
// used by the orchestrator that has to call it, so these two are deliberately open.
// The test asserts they are both registered AND unwrapped, which is what stops this
// list quietly growing to cover a write route: adding a pattern here to silence a
// failure then fails the other half of the same assertion.
var unauthenticatedProbeRoutes = []string{
	"GET /healthz",
	"GET /readyz",
}

// routeRegistration is one mux.Handle/mux.HandleFunc call found in Routes(): the
// pattern it registers, and whether the handler it registers is wrapped in s.auth.
type routeRegistration struct {
	pattern     string
	authWrapped bool
}

// isAuthWrapper reports whether e is the expression s.auth(…) — the bearer-token
// middleware every non-probe route must be registered through. It matches the call
// syntactically rather than by behaviour because that is the point: a route can only
// be checked for a credential if the registration puts the middleware in its path,
// and no HTTP-level probe of one URL can show that some other URL did the same.
func isAuthWrapper(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "auth" {
		return false
	}
	recv, ok := sel.X.(*ast.Ident)
	return ok && recv.Name == "s"
}

// routeHandleCall reports the route a mux.Handle/mux.HandleFunc call registers, and
// whether it is such a call at all. It exists so collectRoutes's walk can filter with
// one call instead of the chained type assertions that recognizing a route
// registration requires.
func routeHandleCall(t *testing.T, call *ast.CallExpr) (routeRegistration, bool) {
	t.Helper()
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc" {
		return routeRegistration{}, false
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok || recv.Name != "mux" {
		return routeRegistration{}, false
	}
	require.Len(t, call.Args, 2,
		"mux.%s takes a pattern and a handler", sel.Sel.Name)
	lit, ok := call.Args[0].(*ast.BasicLit)
	require.True(t, ok && lit.Kind == token.STRING,
		"mux.%s's first argument must be a string literal pattern", sel.Sel.Name)
	pattern, err := strconv.Unquote(lit.Value)
	require.NoError(t, err)
	return routeRegistration{
		pattern: pattern, authWrapped: isAuthWrapper(call.Args[1]),
	}, true
}

// collectRoutes parses server.go and returns every route registered by a
// mux.Handle/mux.HandleFunc call inside the Routes method — the route table
// TestEveryNonProbeRouteRequiresTheBearerToken checks.
func collectRoutes(t *testing.T) []routeRegistration {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err, "parse server.go to inspect Routes()")

	var routes []routeRegistration
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
			if route, matched := routeHandleCall(t, call); matched {
				routes = append(routes, route)
			}
			return true
		})
		return false
	})
	return routes
}

// TestEveryNonProbeRouteRequiresTheBearerToken asserts, structurally, that the bearer
// token guards every write route ingress exposes — not just the one an HTTP-level test
// happens to send an unauthenticated request to.
//
// TestIngressRequiresBearerToken drives POST /v1/events without a token, which proves
// that one path is guarded and says nothing about any other. POST /v1/events/cloudevents
// was in exactly that position: it IS wrapped, but deleting s.auth from its registration
// compiled and broke no test in this suite. Every other CloudEvents test sets
// Authorization before sending, so none of them would ever have noticed, and the same
// would be true of a write route nobody has added yet. Reading the registration is what
// generalizes the property from "this URL requires the token" to "every non-probe route
// does".
//
// It needs no database and runs unconditionally, like TestHealthzWorksWithNilStore: the
// credential boundary on the write side of a system of record should not be a property
// that only a CI leg with a live Postgres can check.
func TestEveryNonProbeRouteRequiresTheBearerToken(t *testing.T) {
	routes := collectRoutes(t)
	require.NotEmpty(t, routes, "Routes() must register at least one handler")

	patterns := make([]string, 0, len(routes))
	for _, r := range routes {
		patterns = append(patterns, r.pattern)
		if slices.Contains(unauthenticatedProbeRoutes, r.pattern) {
			assert.False(t, r.authWrapped,
				"probe %q must stay unauthenticated; the orchestrator that calls it "+
					"holds no token", r.pattern)
			continue
		}
		assert.True(t, r.authWrapped,
			"route %q is registered without s.auth, so no credential guards it; the "+
				"bearer token is the only boundary on the write side of the log",
			r.pattern)
	}
	for _, want := range unauthenticatedProbeRoutes {
		assert.Contains(t, patterns, want,
			"the probes must be registered exactly as documented")
	}
}

// getUnauthenticated drives a GET with no Authorization header, which is how an
// orchestrator calls a probe.
func getUnauthenticated(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// TestReadyzReportsReadyWithLiveDatabase and its 503 counterpart below are ingress's
// readiness coverage, which did not exist: the daemon that accepts writes had no test
// for the probe that decides whether it is in the load balancer at all.
//
// Neither sends an Authorization header, so both also pin that readyz is reachable
// without a credential — an orchestrator holds no token.
func TestReadyzReportsReadyWithLiveDatabase(t *testing.T) {
	h, _ := newServer(t)
	rec := getUnauthenticated(t, h, "/readyz")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestReadyzReports503WhenTheDatabaseIsUnreachable is the half that matters. A readyz
// that only ever answers 200 is worse than none: it keeps a daemon that cannot reach its
// database in rotation, accepting writes it will then fail. The happy path above cannot
// catch that, because a handler returning 200 unconditionally passes it.
//
// Closing the pool is a real outage rather than a faked error, the same technique
// TestPostEventReturns503WhenTheStoreFails uses; pgxpool.Close is guarded by a sync.Once,
// so newServer's own t.Cleanup close is still safe.
func TestReadyzReports503WhenTheDatabaseIsUnreachable(t *testing.T) {
	h, s := newServer(t)
	s.Close()

	rec := getUnauthenticated(t, h, "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "database unreachable",
		"an operator reading the probe's body must be told what is wrong")
}
