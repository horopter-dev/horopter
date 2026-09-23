package ingress_test

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
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/ingress"
	"horopter.dev/horopter/internal/store/postgres"
	"horopter.dev/horopter/internal/testdb"
)

var fixed = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

// The tests share a database that nothing truncates, so every identifier a test puts
// into it has to be one no earlier execution used — otherwise a POST that should create
// a row finds one already there and returns 200 where the test expects 201, failing for
// a reason unrelated to the code under test.
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

// idemKey returns an idempotency key unique to this call. suffix names the call for
// traceability and plays no part in uniqueness.
func idemKey(t *testing.T, suffix string) string {
	t.Helper()
	return uniqueToken() + "-" + t.Name() + "-" + suffix
}

// ingressFixture is a running server plus two credentials tenant now comes from instead
// of the request body: producerKey, bound to producerTenant with locus "github" and no
// subject_pattern, and scopedKey, bound to its own tenant with subject_pattern
// "acme/prod".
type ingressFixture struct {
	srv            http.Handler
	store          *postgres.Store
	producerKey    string
	producerTenant core.TenantID
	scopedKey      string
}

func newIngressFixture(t *testing.T, opts ...postgres.Option) ingressFixture {
	t.Helper()
	v := os.Getenv("HOROPTER_TEST_DSN")
	if v == "" {
		t.Skip("set HOROPTER_TEST_DSN to run ingress tests (docker compose up -d db)")
	}
	s, err := postgres.Open(context.Background(), v, opts...)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	clock := func() time.Time { return fixed }
	authn := auth.New(auth.NewStore(s.Pool()), clock, auth.DefaultOptions())
	srv, err := ingress.New(s, clock, rand.Reader, authn)
	require.NoError(t, err)

	producerTenant, producerKey := seedCredential(t, s, "github", "")
	_, scopedKey := seedCredential(t, s, "github", "acme/prod")

	return ingressFixture{
		srv: srv.Routes(), store: s,
		producerKey: producerKey, producerTenant: producerTenant, scopedKey: scopedKey,
	}
}

// seedCredential inserts a tenant, a principal and a producer credential scoped to locus
// and subjectPattern (empty for none), registering cleanup for all three in the order
// their foreign keys require. It returns the tenant, as the row-backed core.TenantID a
// stored event's tenant is compared against, and the plaintext key SplitKey resolves.
func seedCredential(
	t *testing.T, s *postgres.Store, locus, subjectPattern string,
) (core.TenantID, string) {
	t.Helper()
	key, prefix, hash, err := auth.NewKey(rand.Reader)
	require.NoError(t, err)

	tenantID := testdb.NewTenant(t, "tnt_test_"+prefix)
	principalID := "prn_test_" + prefix
	credentialID := "cred_test_" + prefix
	ctx := context.Background()
	pool := s.Pool()

	_, err = pool.Exec(ctx, `INSERT INTO principal (id, display_name) VALUES ($1, $1)`,
		principalID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM principal WHERE id = $1`, principalID)
	})

	var pattern *string
	if subjectPattern != "" {
		pattern = &subjectPattern
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO credential (id, principal_id, type, key_prefix, key_hash, role,
		                        tenant_id, source, locus, subject_pattern)
		VALUES ($1, $2, 'machine_key', $3, $4, 'producer', $5, 'test', $6, $7)`,
		credentialID, principalID, prefix, hash, tenantID, locus, pattern)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM credential WHERE id = $1`, credentialID)
	})

	return core.TenantIDFromRow(tenantID), key
}

func doPost(
	t *testing.T, h http.Handler, path, contentType, key, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", contentType)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func post(t *testing.T, h http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doPost(t, h, "/v1/events", "application/json", key, body)
}

func postCE(t *testing.T, h http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doPost(t, h, "/v1/events/cloudevents", "application/cloudevents+json", key, body)
}

// storedRow is the columns of a stored event that never appear in the 201 response, so
// tests that need them read the row back rather than parsing the body a second way.
type storedRow struct {
	Tenant         string
	CorrelationRef string
	Origin         core.Origin
	SchemaVersion  int
}

// storedEvent reads back the row rec's body names by id. Callers that care about the
// response status check it themselves first, so a wrong status fails at its own
// assertion rather than being masked by a confusing "no rows" from this helper.
func storedEvent(t *testing.T, store *postgres.Store, rec *httptest.ResponseRecorder) storedRow {
	t.Helper()
	var resp struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	var row storedRow
	var origin string
	err := store.Pool().QueryRow(context.Background(),
		`SELECT tenant, correlation_ref, origin, schema_version FROM event WHERE id = $1`,
		resp.ID,
	).Scan(&row.Tenant, &row.CorrelationRef, &origin, &row.SchemaVersion)
	require.NoError(t, err)
	row.Origin = core.Origin(origin)
	return row
}

const validBody = `{"source":"s","type":"t.started","subject":"acme/prod",
	"occurred_at":"2026-09-22T00:00:00Z"}`

// bodyWith appends extra top-level JSON fields to validBody's object.
func bodyWith(extra string) string {
	return fmt.Sprintf(`{"source":"s","type":"t.started","subject":"acme/prod",
		"occurred_at":"2026-09-22T00:00:00Z",%s}`, extra)
}

func bodyWithRef(ref string) string {
	return fmt.Sprintf(`{"source":"s","type":"t.started","subject":"acme/prod",
		"occurred_at":"2026-09-22T00:00:00Z","correlation_ref":%q}`, ref)
}

func bodySubject(subject string) string {
	return fmt.Sprintf(`{"source":"s","type":"t.started","subject":%q,
		"occurred_at":"2026-09-22T00:00:00Z"}`, subject)
}

func TestPostRejectsABodyCarryingTenant(t *testing.T) {
	f := newIngressFixture(t)
	rec := post(t, f.srv, f.producerKey, `{"tenant":"other","source":"s","type":"t.started",
		"correlation_ref":"r","occurred_at":"2026-09-22T00:00:00Z"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "tenant is derived from the credential")
}

func TestPostStoresTheTenantFromTheCredential(t *testing.T) {
	f := newIngressFixture(t)
	rec := post(t, f.srv, f.producerKey, validBody)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, f.producerTenant.String(), storedEvent(t, f.store, rec).Tenant)
}

func TestPostPrefixesTheRefWithTheCredentialLocus(t *testing.T) {
	f := newIngressFixture(t) // producerKey's locus is "github"
	rec := post(t, f.srv, f.producerKey, bodyWithRef("deploy-1"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "github/deploy-1", storedEvent(t, f.store, rec).CorrelationRef,
		"the response echoes the stored ref, which is what a query needs")
}

func TestPostSetsOriginFromTheCredentialType(t *testing.T) {
	f := newIngressFixture(t)
	rec := post(t, f.srv, f.producerKey, validBody)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, core.OriginProcess, storedEvent(t, f.store, rec).Origin)
}

func TestPostIgnoresOriginInTheBody(t *testing.T) {
	f := newIngressFixture(t)
	rec := post(t, f.srv, f.producerKey, bodyWith(`"origin":"human"`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, core.OriginProcess, storedEvent(t, f.store, rec).Origin,
		"origin is server-set; a caller-asserted value would be worthless")
}

func TestSubjectPatternIsEnforced(t *testing.T) {
	f := newIngressFixture(t)
	assert.Equal(t, http.StatusCreated,
		post(t, f.srv, f.scopedKey, bodySubject("acme/prod")).Code)
	assert.Equal(t, http.StatusForbidden,
		post(t, f.srv, f.scopedKey, bodySubject("acme/dev")).Code)
}

func TestAnEmptySubjectDoesNotMatchAPattern(t *testing.T) {
	f := newIngressFixture(t)
	assert.Equal(t, http.StatusForbidden, post(t, f.srv, f.scopedKey, bodySubject("")).Code,
		"an absent subject must not satisfy a narrowing pattern by accident")
}

func TestRefAtTheCapIsAcceptedAndOneOverIsRejected(t *testing.T) {
	f := newIngressFixture(t)
	// The locus prefix counts toward the stored ref, so the cap applies to what is stored.
	atCap := strings.Repeat("x", auth.MaxRefBytes-len("github/"))
	assert.Equal(t, http.StatusCreated, post(t, f.srv, f.producerKey, bodyWithRef(atCap)).Code)
	assert.Equal(t, http.StatusBadRequest,
		post(t, f.srv, f.producerKey, bodyWithRef(atCap+"x")).Code)
}

func TestStoredEventsCarrySchemaVersionTwo(t *testing.T) {
	f := newIngressFixture(t)
	assert.Equal(t, 2, core.CurrentSchemaVersion)
	rec := post(t, f.srv, f.producerKey, validBody)
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, 2, storedEvent(t, f.store, rec).SchemaVersion)
}

func TestPostEventCreatesChange(t *testing.T) {
	f := newIngressFixture(t)
	body := fmt.Sprintf(`{"source":"buildkite","type":"deployment.started",
		"subject":"acme/prod","actor":"ci-bot","occurred_at":"2026-09-14T10:00:00Z",
		"correlation_ref":"dep-7","idempotency_key":%q,
		"attributes":{"service.version":"2.5.0"}}`, idemKey(t, "1"))
	rec := post(t, f.srv, f.producerKey, body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"ref":"github/dep-7"`)
}

// TestPostEventRecordsTheTenantOnTheSpan pins horopter.tenant on the request's server
// span. It must be set once per request, on the span that exists for every request,
// rather than only on the ones that reach the store.
//
// The loop below must skip the store's own "AppendEvent" span, which carries the same
// horopter.tenant attribute (internal/store/postgres/outcome.go), and this
// request reaches the store, so both spans land in one recorder carrying the attribute
// this test checks for. Scanning every span for it — as an earlier version of this test
// did — proves nothing about recordTenant specifically: deleting its call in postEvent
// would still leave AppendEvent's own attribute for this loop to find, and the test
// would stay green over a dead guard.
func TestPostEventRecordsTheTenantOnTheSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	f := newIngressFixture(t)
	resp := post(t, f.srv, f.producerKey, validBody)
	require.Equal(t, http.StatusCreated, resp.Code)

	var found bool
	for _, span := range rec.Ended() {
		if span.Name() == "AppendEvent" {
			continue
		}
		for _, kv := range span.Attributes() {
			if kv.Key == "horopter.tenant" && kv.Value.AsString() == f.producerTenant.String() {
				found = true
			}
		}
	}
	assert.True(t, found, "the request's tenant must be recorded on the server span")
}

func TestPostDuplicateIdempotencyKeyReturns200(t *testing.T) {
	f := newIngressFixture(t)
	body := bodyWith(fmt.Sprintf(`"idempotency_key":%q`, idemKey(t, "dup")))
	rec1 := post(t, f.srv, f.producerKey, body)
	require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
	assert.Equal(t, http.StatusOK, post(t, f.srv, f.producerKey, body).Code,
		"a CI retry is normal traffic, not an error")
}

func TestPostInvalidEventReturns400WithReason(t *testing.T) {
	f := newIngressFixture(t)
	body := `{"source":"ci","type":"deployment.started","occurred_at":"2026-09-14T10:00:00Z"}`
	rec := post(t, f.srv, f.producerKey, body)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "subject")
}

func TestIngressRequiresACredential(t *testing.T) {
	f := newIngressFixture(t)
	assert.Equal(t, http.StatusUnauthorized, post(t, f.srv, "", validBody).Code)
	assert.Equal(t, http.StatusUnauthorized, post(t, f.srv, "wrong", validBody).Code)
}

func TestHealthzNeedsNoCredential(t *testing.T) {
	f := newIngressFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "probes must not require credentials")
}

func TestCloudEventIngest(t *testing.T) {
	f := newIngressFixture(t)
	body := fmt.Sprintf(`{"specversion":"1.0","id":%q,"source":"/buildkite/pipelines/api",
		"type":"deployment.started","subject":"acme/prod","time":"2026-09-14T10:00:00Z",
		"datacontenttype":"application/json",
		"data":{"correlation_ref":"dep-7","service.version":"2.5.0"}}`, idemKey(t, "ce"))
	rec := postCE(t, f.srv, f.producerKey, body)
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
	clock := func() time.Time { return fixed }
	authn := auth.New(nil, clock, auth.DefaultOptions())
	srv, err := ingress.New(nil, clock, rand.Reader, authn)
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
	f := newIngressFixture(t)
	body := `{"specversion":"1.0","source":"/buildkite/pipelines/api",
		"type":"deployment.started","subject":"acme/prod","time":"2026-09-14T10:00:00Z",
		"datacontenttype":"application/json",
		"data":{"correlation_ref":"dep-7"}}`
	rec := postCE(t, f.srv, f.producerKey, body)
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
	f := newIngressFixture(t)
	body := fmt.Sprintf(`{"specversion":"1.0","id":%q,"source":"/buildkite/pipelines/api",
		"type":"deployment.started","subject":"acme/prod","time":"2026-09-14T10:00:00Z",
		"datacontenttype":"application/json",
		"data":{"correlation_ref":"dep-7","service.version":"2.5.0"}}`, idemKey(t, "ce-dup"))
	send := func() *httptest.ResponseRecorder { return postCE(t, f.srv, f.producerKey, body) }
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
// The credential is resolved once, successfully, before the store closes: the
// authenticator shares the store's own pool, so an unwarmed cache would answer 503 from
// the credential lookup instead of from the append this test means to exercise. Closing
// the pool afterwards is a real transient store failure rather than a faked error, and
// pgxpool.Close is guarded by a sync.Once, so newIngressFixture's own t.Cleanup close is
// still safe.
func TestPostEventReturns503WhenTheStoreFails(t *testing.T) {
	f := newIngressFixture(t)
	warm := post(t, f.srv, f.producerKey, bodyWith(fmt.Sprintf(`"idempotency_key":%q`,
		idemKey(t, "warm"))))
	require.Equal(t, http.StatusCreated, warm.Code, warm.Body.String())
	f.store.Close()

	body := bodyWith(fmt.Sprintf(`"idempotency_key":%q`, idemKey(t, "down")))
	rec := post(t, f.srv, f.producerKey, body)

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
	f := newIngressFixture(t)
	sharedID := idemKey(t, "shared-across-sources")

	send := func(source string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"specversion":"1.0","id":%q,"source":%q,
			"type":"deployment.started","subject":"acme/prod","time":"2026-09-14T10:00:00Z",
			"datacontenttype":"application/json",
			"data":{"correlation_ref":"dep-7"}}`, sharedID, source)
		return postCE(t, f.srv, f.producerKey, body)
	}

	first := send("/buildkite/pipelines/api")
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	second := send("/github/actions/api")
	require.Equal(t, http.StatusCreated, second.Code, second.Body.String(),
		"two sources sharing a CloudEvents id are two distinct events; keying on the id "+
			"alone swallows the second and reports it as a retry")

	evs, _, err := f.store.ListEvents(context.Background(),
		postgres.EventQuery{Tenant: f.producerTenant.String()})
	require.NoError(t, err)
	assert.Len(t, evs, 2, "both events must be in the log, not one")
}

// TestNewRejectsANilAuthenticator pins the guard where it is structural rather than
// remembered: every write route is gated on the authenticator, so a Server built
// without one would either panic on the first request or need every caller to
// remember the check itself.
//
// It runs unconditionally: a constructor that refuses its argument needs no database.
func TestNewRejectsANilAuthenticator(t *testing.T) {
	clock := func() time.Time { return fixed }
	srv, err := ingress.New(nil, clock, rand.Reader, nil)
	require.Error(t, err, "a nil authenticator would panic on the first request rather "+
		"than reject it")
	assert.Nil(t, srv, "a rejected configuration must not yield a usable server")
}

// TestPostEventRejectsAnOversizedBody covers the body cap on both readers. Neither reader
// had one: json.Decode and io.ReadAll both consume the whole body before anything is
// validated, so a single request decided how much the process allocated. 413 rather than
// 400 is the honest status — the body may be perfectly well formed and merely too large —
// and reporting it as malformed JSON would send the caller hunting for a syntax error
// that does not exist.
func TestPostEventRejectsAnOversizedBody(t *testing.T) {
	f := newIngressFixture(t)
	huge := fmt.Sprintf(`{"source":"ci","type":"deployment.started",
		"subject":"acme/prod","occurred_at":"2026-09-14T10:00:00Z",
		"attributes":{"blob":%q}}`, strings.Repeat("x", ingress.MaxBodyBytes+1))

	for path, contentType := range map[string]string{
		"/v1/events":             "application/json",
		"/v1/events/cloudevents": "application/cloudevents+json",
	} {
		t.Run(path, func(t *testing.T) {
			rec := doPost(t, f.srv, path, contentType, f.producerKey, huge)
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "exceeds",
				"the caller must be told the body was too large, not that it was malformed")
		})
	}
}

// unauthenticatedProbeRoutes is TestEveryNonProbeRouteRequiresACredential's documented
// exception list to "every route is registered through the authenticator": the liveness
// and readiness probes. A probe that demanded a credential could not be used by the
// orchestrator that has to call it, so these two are deliberately open.
var unauthenticatedProbeRoutes = []string{
	"GET /healthz",
	"GET /readyz",
}

// routeRegistration is one mux.Handle/mux.HandleFunc call found in Routes(): the
// pattern it registers, and whether the handler it registers is wrapped in the
// authenticator's Require middleware.
type routeRegistration struct {
	pattern     string
	authWrapped bool
}

// isAuthWrapper reports whether e is the expression s.authn.Require(...)(...) — the
// credential middleware every non-probe route must be registered through. It matches the
// call syntactically rather than by behaviour because that is the point: a route can
// only be checked for a credential if the registration puts the middleware in its path,
// and no HTTP-level probe of one URL can show that some other URL did the same.
func isAuthWrapper(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	inner, ok := call.Fun.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := inner.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Require" {
		return false
	}
	recvSel, ok := sel.X.(*ast.SelectorExpr)
	if !ok || recvSel.Sel.Name != "authn" {
		return false
	}
	recv, ok := recvSel.X.(*ast.Ident)
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
// TestEveryNonProbeRouteRequiresACredential checks.
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

// TestEveryNonProbeRouteRequiresACredential asserts, structurally, that a credential
// guards every write route ingress exposes — not just the one an HTTP-level test
// happens to send an unauthenticated request to.
//
// TestIngressRequiresACredential drives POST /v1/events without a credential, which
// proves that one path is guarded and says nothing about any other. POST
// /v1/events/cloudevents was in exactly that position: it IS wrapped, but deleting the
// middleware from its registration compiled and broke no test in this suite. Every other
// CloudEvents test sets Authorization before sending, so none of them would ever have
// noticed, and the same would be true of a write route nobody has added yet. Reading the
// registration is what generalizes the property from "this URL requires a credential" to
// "every non-probe route does".
//
// It needs no database and runs unconditionally, like TestHealthzWorksWithNilStore: the
// credential boundary on the write side of a system of record should not be a property
// that only a CI leg with a live Postgres can check.
func TestEveryNonProbeRouteRequiresACredential(t *testing.T) {
	routes := collectRoutes(t)
	require.NotEmpty(t, routes, "Routes() must register at least one handler")

	patterns := make([]string, 0, len(routes))
	for _, r := range routes {
		patterns = append(patterns, r.pattern)
		if slices.Contains(unauthenticatedProbeRoutes, r.pattern) {
			assert.False(t, r.authWrapped,
				"probe %q must stay unauthenticated; the orchestrator that calls it "+
					"holds no credential", r.pattern)
			continue
		}
		assert.True(t, r.authWrapped,
			"route %q is registered without the authenticator, so no credential guards "+
				"it; the credential is the only boundary on the write side of the log",
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
	f := newIngressFixture(t)
	rec := getUnauthenticated(t, f.srv, "/readyz")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestReadyzReports503WhenTheDatabaseIsUnreachable is the half that matters. A readyz
// that only ever answers 200 is worse than none: it keeps a daemon that cannot reach its
// database in rotation, accepting writes it will then fail. The happy path above cannot
// catch that, because a handler returning 200 unconditionally passes it.
//
// Closing the pool is a real outage rather than a faked error, the same technique
// TestPostEventReturns503WhenTheStoreFails uses; pgxpool.Close is guarded by a sync.Once,
// so newIngressFixture's own t.Cleanup close is still safe.
func TestReadyzReports503WhenTheDatabaseIsUnreachable(t *testing.T) {
	f := newIngressFixture(t)
	f.store.Close()

	rec := getUnauthenticated(t, f.srv, "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "database unreachable",
		"an operator reading the probe's body must be told what is wrong")
}

// captureLogs sends slog's default logger to a buffer until the test ends, and returns a
// function decoding every record written so far. Swapping a process-wide default is safe
// only because no test in this package calls t.Parallel.
//
// This is a copy of internal/egress/server_test.go's helper of the same name, not an
// import: both files are external test packages, so neither can import the other.
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

// TestReadyzDoesNotRelayTheDatabaseError pins what the 503 body withholds. The probe sits
// outside the credential check — an orchestrator has no credential to send — and pgx
// formats a failed connection with the database user, database name, host and port, so
// relaying the error handed all four to anyone who could reach the port during an
// outage. That contradicted appendFailed's rule on the same daemon.
func TestReadyzDoesNotRelayTheDatabaseError(t *testing.T) {
	f := newIngressFixture(t)
	f.store.Close()

	rec := getUnauthenticated(t, f.srv, "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "closed pool",
		"the database error must not reach an unauthenticated caller")
}

// TestFailedReadinessProbeLogsItsCause is the other half: withholding the error from the
// body must not lose it. A pod that drops out of rotation with no record of why leaves an
// operator reproducing the probe by hand to find out.
func TestFailedReadinessProbeLogsItsCause(t *testing.T) {
	f := newIngressFixture(t)
	f.store.Close()
	records := captureLogs(t)

	rec := getUnauthenticated(t, f.srv, "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())

	logged := slices.ContainsFunc(records(), func(r map[string]any) bool {
		cause, _ := r["error"].(string)
		return r["level"] == "ERROR" && strings.Contains(cause, "closed pool")
	})
	assert.True(t, logged, "the probe's failure must reach the server log; got %v", records())
}

// TestPostEventRefusesAttributesOverTheCap drives the cap through the handler: the store
// refuses the event as invalid, so the producer gets a 400 naming the size and the limit, not
// a retryable 503.
func TestPostEventRefusesAttributesOverTheCap(t *testing.T) {
	f := newIngressFixture(t, postgres.WithMaxEventAttributeBytes(26))
	body := fmt.Sprintf(`{"source":"buildkite","type":"deployment.started",
		"subject":"acme/prod","occurred_at":"2026-09-14T10:00:00Z",
		"idempotency_key":%q,"attributes":{"service.version":"2.5.0"}}`, idemKey(t, "1"))

	rec := post(t, f.srv, f.producerKey, body)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(),
		"attributes encode to 27 bytes of JSON; the limit is 26")
}
