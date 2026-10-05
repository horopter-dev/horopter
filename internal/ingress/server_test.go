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

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/ingress"
	"horopter.dev/horopter/internal/logging"
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
	return newIngressFixtureOpts(t, opts, nil)
}

// newIngressFixtureWith builds the same fixture with ingress options, for the handful of
// tests that exercise a non-default server configuration.
func newIngressFixtureWith(t *testing.T, srvOpts ...ingress.Option) ingressFixture {
	t.Helper()
	return newIngressFixtureOpts(t, nil, srvOpts)
}

// newIngressFixtureOpts is the one body both constructors share, so a change to the fixture
// cannot apply to only one of them.
func newIngressFixtureOpts(
	t *testing.T, storeOpts []postgres.Option, srvOpts []ingress.Option,
) ingressFixture {
	t.Helper()
	v := os.Getenv("HOROPTER_TEST_DSN")
	if v == "" {
		t.Skip("set HOROPTER_TEST_DSN to run ingress tests (docker compose up -d db)")
	}
	s, err := postgres.Open(context.Background(), v, storeOpts...)
	require.NoError(t, err)
	t.Cleanup(s.Close)

	clock := func() time.Time { return fixed }
	authn := auth.New(auth.NewStore(s.Pool()), clock, auth.DefaultOptions())
	// Monotonic entropy, not rand.Reader directly: clock is frozen at fixed, so every
	// event a test appends shares one ULID timestamp prefix, and plain crypto/rand
	// entropy would give two same-test events an arbitrary relative id order — the exact
	// condition applyToProjection now treats as an out-of-order append. See
	// internal/store/postgres/append_test.go's monotonicEntropy for the identical hazard.
	// This one is a fresh ulid.Monotonic built per fixture, not a package-level variable
	// shared across goroutines, so monotonicEntropy's own concurrent-use caveat does not
	// carry over: two ingress fixtures never share this instance.
	srv, err := ingress.New(s, clock, ulid.Monotonic(rand.Reader, 0), authn, srvOpts...)
	require.NoError(t, err)

	producerTenant, producerKey := seedCredential(t, s, "github", "")
	_, scopedKey := seedCredential(t, s, "github", "acme/prod")

	return ingressFixture{
		srv: srv.Routes(), store: s,
		producerKey: producerKey, producerTenant: producerTenant, scopedKey: scopedKey,
	}
}

// credentialSource is the source every fixture credential is issued with. It differs from
// every source a test body or envelope names, so a stored event shows which one won.
const credentialSource = "test"

// seedCredential inserts a tenant, a principal and a producer credential scoped to locus
// and subjectPattern (empty for none), registering cleanup for all three in the order
// their foreign keys require. It returns the tenant, as the row-backed core.TenantID a
// stored event's tenant is compared against, and the plaintext key SplitKey resolves.
//
// The insert goes through s's own pool, live for the call; the cleanup goes through
// testdb's shared pool instead, since several tests in this package close s's pool before
// the test ends, and a cleanup racing that closed pool would silently leave the row behind
// for testdb's own tenant cleanup to trip over as a foreign-key violation.
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
	t.Cleanup(func() { deleteRow(t, "principal", principalID) })

	var pattern *string
	if subjectPattern != "" {
		pattern = &subjectPattern
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO credential (id, principal_id, type, key_prefix, key_hash, role,
		                        tenant_id, source, locus, subject_pattern)
		VALUES ($1, $2, 'machine_key', $3, $4, 'producer', $5, $6, $7, $8)`,
		credentialID, principalID, prefix, hash, tenantID, credentialSource, locus, pattern)
	require.NoError(t, err)
	t.Cleanup(func() { deleteRow(t, "credential", credentialID) })

	return core.TenantIDFromRow(tenantID), key
}

// seedGlobalCredential inserts a principal and a credential of role with tenant_id left
// NULL — the shape `horoctl admin keys issue --role producer` produces with --tenant
// omitted, and the shape --role superuser always produces, since credential.tenant_id is
// nullable. Mirrors seedCredential above, differing only in the tenant column and the
// caller-chosen role in place of a fixed "producer".
func seedGlobalCredential(t *testing.T, s *postgres.Store, role string) string {
	t.Helper()
	key, prefix, hash, err := auth.NewKey(rand.Reader)
	require.NoError(t, err)

	principalID := "prn_test_" + prefix
	credentialID := "cred_test_" + prefix
	ctx := context.Background()
	pool := s.Pool()

	_, err = pool.Exec(ctx, `INSERT INTO principal (id, display_name) VALUES ($1, $1)`,
		principalID)
	require.NoError(t, err)
	t.Cleanup(func() { deleteRow(t, "principal", principalID) })

	_, err = pool.Exec(ctx, `
		INSERT INTO credential (id, principal_id, type, key_prefix, key_hash, role,
		                        tenant_id, source, locus)
		VALUES ($1, $2, 'machine_key', $3, $4, $5, NULL, $6, 'test')`,
		credentialID, principalID, prefix, hash, role, credentialSource)
	require.NoError(t, err)
	t.Cleanup(func() { deleteRow(t, "credential", credentialID) })

	return key
}

// eventCountBySubject counts event rows carrying subject, for a test that must prove a
// rejected append wrote nothing rather than trusting the response code alone.
func eventCountBySubject(t *testing.T, store *postgres.Store, subject string) int {
	t.Helper()
	var n int
	require.NoError(t, store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM event WHERE subject = $1`, subject).Scan(&n))
	return n
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
//
// The column list below is the same narrow-SELECT hazard rebuild.go's foldEventLog
// warns about: a column absent from a SELECT is a column no test reading through this
// helper can observe going wrong, however wrong production leaves it. Locus was added
// here for exactly that reason — it used to be missing, and no native-route test could
// have caught a wrong-but-valid locus reaching the stored row.
type storedRow struct {
	Tenant         string
	Source         string
	IdempotencyKey string
	CorrelationRef string
	Origin         core.Origin
	Locus          string
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
		`SELECT tenant, source, COALESCE(idempotency_key, ''), correlation_ref, origin, locus,
		        schema_version
		   FROM event WHERE id = $1`,
		resp.ID,
	).Scan(&row.Tenant, &row.Source, &row.IdempotencyKey, &row.CorrelationRef, &origin,
		&row.Locus, &row.SchemaVersion)
	require.NoError(t, err)
	row.Origin = core.Origin(origin)
	return row
}

const validBody = `{"type":"t.started","subject":"acme/prod",
	"occurred_at":"2026-09-22T00:00:00Z"}`

// bodyWith appends extra top-level JSON fields to validBody's object.
func bodyWith(extra string) string {
	return fmt.Sprintf(`{"type":"t.started","subject":"acme/prod",
		"occurred_at":"2026-09-22T00:00:00Z",%s}`, extra)
}

func bodyWithRef(ref string) string {
	return fmt.Sprintf(`{"type":"t.started","subject":"acme/prod",
		"occurred_at":"2026-09-22T00:00:00Z","correlation_ref":%q}`, ref)
}

func bodySubject(subject string) string {
	return fmt.Sprintf(`{"type":"t.started","subject":%q,
		"occurred_at":"2026-09-22T00:00:00Z"}`, subject)
}

// bodyCE builds a minimal CloudEvents envelope for the given subject, omitting the subject
// attribute entirely when it is empty rather than sending it as "". The CloudEvents spec —
// and this SDK's Validate — rejects a PRESENT-but-empty subject as malformed ("if present,
// MUST be a non-empty string"), so an envelope carrying no subject at all is the only way
// to drive the "absent subject" case as far as subjectAllowed instead of failing earlier
// as a 400.
func bodyCE(t *testing.T, subject string) string {
	t.Helper()
	var subjectField string
	if subject != "" {
		subjectField = fmt.Sprintf(`,"subject":%q`, subject)
	}
	return fmt.Sprintf(`{"specversion":"1.0","id":%q,"source":"/buildkite/pipelines/api",
		"type":"deployment.started","time":"2026-09-14T10:00:00Z",
		"datacontenttype":"application/json","data":{}%s}`,
		idemKey(t, "ce-subject"), subjectField)
}

func TestPostRejectsABodyCarryingTenant(t *testing.T) {
	f := newIngressFixture(t)
	rec := post(t, f.srv, f.producerKey, `{"tenant":"other","type":"t.started",
		"correlation_ref":"r","occurred_at":"2026-09-22T00:00:00Z"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "tenant is derived from the credential")
}

// TestPostRejectsABodyCarryingANullTenant pins that a present-but-null "tenant" is
// rejected exactly like a present-and-populated one. A body is caller-controlled, so a
// producer sending the key at all — whatever its value — must be told, not just one
// sending a non-empty value; treating null as "absent" is how a string-typed field would
// have silently let a null-tenant body through the same check that catches every other
// value.
func TestPostRejectsABodyCarryingANullTenant(t *testing.T) {
	f := newIngressFixture(t)
	rec := post(t, f.srv, f.producerKey, `{"tenant":null,"type":"t.started",
		"subject":"acme/prod","occurred_at":"2026-09-22T00:00:00Z"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "tenant is derived from the credential")
}

func TestPostRejectsABodyCarryingSource(t *testing.T) {
	f := newIngressFixture(t)
	for name, value := range map[string]string{
		"populated": `"buildkite"`, "empty": `""`, "null": `null`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := post(t, f.srv, f.producerKey, fmt.Sprintf(`{"source":%s,"type":"t.started",
				"subject":"acme/prod","occurred_at":"2026-09-22T00:00:00Z"}`, value))
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "source is derived from the credential")
		})
	}
}

func TestPostStoresTheSourceFromTheCredential(t *testing.T) {
	f := newIngressFixture(t)
	rec := post(t, f.srv, f.producerKey, validBody)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, credentialSource, storedEvent(t, f.store, rec).Source)
}

// TestCloudEventPostStoresTheSourceFromTheCredential pins both roles the envelope's source
// had: Event.Source now comes from the credential, while the envelope source stays half of
// the idempotency key, which is CloudEvents' own identity for the event.
func TestCloudEventPostStoresTheSourceFromTheCredential(t *testing.T) {
	f := newIngressFixture(t)
	rec := postCE(t, f.srv, f.producerKey, bodyCE(t, "acme/prod"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	row := storedEvent(t, f.store, rec)
	assert.Equal(t, credentialSource, row.Source)
	assert.Contains(t, row.IdempotencyKey, "/buildkite/pipelines/api/")
}

func TestPostStoresTheTenantFromTheCredential(t *testing.T) {
	f := newIngressFixture(t)
	rec := post(t, f.srv, f.producerKey, validBody)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, f.producerTenant.String(), storedEvent(t, f.store, rec).Tenant)
}

// TestPostRejectsAGlobalAppendCredential and TestCloudEventPostRejectsAGlobalAppendCredential
// pin that a tenant-less append credential — horoctl admin keys issue --role producer
// with --tenant omitted, or any --role superuser credential, since credential.tenant_id
// is nullable — is refused by core.Event.Validate's tenant check, not stamped some other
// way. Tenant is set to p.Tenant.String(), which is "" for such a credential, and
// Validate rejects an empty tenant with a 400.
//
// Egress pins the read-side equivalent, TestASuperuserNamingNoTenantIsRejected in
// internal/egress; ingress had no counterpart on either route. The two routes are
// separate handlers that each stamp Tenant from the credential independently, and this
// package has already been bitten once by a guard exercised on the native route and
// merely assumed on CloudEvents — subjectAllowed had no CloudEvents-route test until
// TestSubjectPatternIsEnforcedOnCloudEvents was added — so each route gets its own pin
// here rather than one standing in for both.
func TestPostRejectsAGlobalAppendCredential(t *testing.T) {
	f := newIngressFixture(t)
	key := seedGlobalCredential(t, f.store, "superuser")
	subject := idemKey(t, "global-native")

	rec := post(t, f.srv, key, bodySubject(subject))

	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "tenant is required")
	assert.Equal(t, 0, eventCountBySubject(t, f.store, subject),
		"a rejected append must write no event row")
}

func TestCloudEventPostRejectsAGlobalAppendCredential(t *testing.T) {
	f := newIngressFixture(t)
	key := seedGlobalCredential(t, f.store, "superuser")
	subject := idemKey(t, "global-ce")

	rec := postCE(t, f.srv, key, bodyCE(t, subject))

	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "tenant is required")
	assert.Equal(t, 0, eventCountBySubject(t, f.store, subject),
		"a rejected append must write no event row")
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

// TestPostSetsLocusFromTheCredential pins the value ev.Locus is stamped with, not merely
// its presence. TestPostPrefixesTheRefWithTheCredentialLocus reads like it covers this
// and does not: it exercises resolveRef's prefix on correlation_ref, a mechanism that
// never touches ev.Locus, so a wrong-but-valid locus stamped at the ingest site would
// leave that test, and every other test in this file, green.
func TestPostSetsLocusFromTheCredential(t *testing.T) {
	f := newIngressFixture(t) // producerKey's locus is "github"
	rec := post(t, f.srv, f.producerKey, validBody)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "github", storedEvent(t, f.store, rec).Locus)
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

// TestSubjectPatternIsEnforcedOnCloudEvents mirrors TestSubjectPatternIsEnforced for the
// CloudEvents route. postCloudEvent calls the same subjectAllowed guard as postEvent, in
// the same order, but nothing before this drove it through the HTTP handler: every other
// CloudEvents test in this suite uses the unscoped producer credential, and
// cloudevents_test.go calls eventFromCloudEvent directly rather than going through
// ServeHTTP. Without this, a regression deleting the guard from postCloudEvent alone would
// pass the entire suite.
func TestSubjectPatternIsEnforcedOnCloudEvents(t *testing.T) {
	f := newIngressFixture(t)
	assert.Equal(t, http.StatusCreated,
		postCE(t, f.srv, f.scopedKey, bodyCE(t, "acme/prod")).Code)
	assert.Equal(t, http.StatusForbidden,
		postCE(t, f.srv, f.scopedKey, bodyCE(t, "acme/dev")).Code)
}

// TestAnEmptySubjectDoesNotMatchAPatternOnCloudEvents mirrors
// TestAnEmptySubjectDoesNotMatchAPattern for the CloudEvents route.
func TestAnEmptySubjectDoesNotMatchAPatternOnCloudEvents(t *testing.T) {
	f := newIngressFixture(t)
	assert.Equal(t, http.StatusForbidden, postCE(t, f.srv, f.scopedKey, bodyCE(t, "")).Code,
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

// TestPostRejectsARefWithLeadingWhitespaceEvenAfterTheLocusPrefix pins that the ref's
// whitespace rule is checked before it is composed with the locus, not after. Composing
// first moves a leading space away from the string's edge — "github/ dep-1" is not
// caught by the same check that rejects " dep-1" — which would silently admit the one
// producer mistake that check exists to catch: an untrimmed ref posts a start under one
// ref and its completion under another, and nothing errors.
func TestPostRejectsARefWithLeadingWhitespaceEvenAfterTheLocusPrefix(t *testing.T) {
	f := newIngressFixture(t)
	rec := post(t, f.srv, f.producerKey, bodyWithRef(" deploy-1"))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "whitespace")
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
	body := fmt.Sprintf(`{"type":"deployment.started",
		"subject":"acme/prod","actor":"ci-bot","occurred_at":"2026-09-14T10:00:00Z",
		"correlation_ref":"dep-7","idempotency_key":%q,
		"attributes":{"service.version":"2.5.0"}}`, idemKey(t, "1"))
	rec := post(t, f.srv, f.producerKey, body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"ref":"github/dep-7"`)
}

// TestPostEventAcceptsALifecycleAssertion pins the native route's new field end to end. The
// type's suffix asserts NOTHING, so the change can only open because the explicit value was
// read — asserting that the request is merely accepted would pass with the field dropped.
func TestPostEventAcceptsALifecycleAssertion(t *testing.T) {
	f := newIngressFixture(t)
	body := fmt.Sprintf(`{"type":"deployment.initiated",
		"subject":"acme/prod","occurred_at":"2026-09-22T00:00:00Z",
		"lifecycle":"started","idempotency_key":%q}`, idemKey(t, "1"))
	rec := post(t, f.srv, f.producerKey, body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	ref := got["ref"].(string)

	var status string
	require.NoError(t, f.store.Pool().QueryRow(context.Background(),
		`SELECT status FROM change WHERE tenant = $1 AND ref = $2`,
		f.producerTenant.String(), ref).Scan(&status))
	assert.Equal(t, "in_progress", status,
		"deployment.initiated asserts nothing by suffix, so in_progress can only come "+
			"from the explicit lifecycle")

	// The store's read path carries lifecycle too, and nothing else here asserts it: a
	// coordinated removal from both eventColumns and scanEvents' dest would leave the
	// column written but never read back, and no other test would notice.
	q, err := postgres.NewEventQuery(f.producerTenant, 0)
	require.NoError(t, err)
	evs, _, err := f.store.ListEvents(context.Background(), q)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, core.LifecycleStarted, evs[0].Lifecycle,
		"the stored lifecycle must round-trip through the store's own read path")
}

// TestPostEventRefusesAnUnknownLifecycle keeps a wrong value a 400 with a useful message
// rather than a 500 carrying a Postgres constraint name. "Started" is the Review Focus
// case: right word, wrong case, and the vocabulary is exact.
func TestPostEventRefusesAnUnknownLifecycle(t *testing.T) {
	f := newIngressFixture(t)
	body := fmt.Sprintf(`{"type":"deployment.initiated",
		"subject":"acme/prod","occurred_at":"2026-09-22T00:00:00Z",
		"lifecycle":"Started","idempotency_key":%q}`, idemKey(t, "1"))
	rec := post(t, f.srv, f.producerKey, body)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Started", "the error must name the value")
	assert.Contains(t, rec.Body.String(), "started", "and list what is permitted")
}

// TestAmendHandleRoundTrips is the assertion the ref/id fork needs, and it has to ECHO the
// value rather than inspect it: a test asserting the response's shape passes while the
// documented handle still forks.
//
// One change with both events is the discriminating assertion. Echoing the stored ref
// instead composes a second locus prefix and creates a SECOND change, which no
// single-change assertion would notice.
func TestAmendHandleRoundTrips(t *testing.T) {
	f := newIngressFixture(t)
	open := fmt.Sprintf(`{"type":"deployment.started",
		"subject":"acme/prod","occurred_at":"2026-09-22T00:00:00Z",
		"idempotency_key":%q}`, idemKey(t, "open"))
	rec := post(t, f.srv, f.producerKey, open)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var opened map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &opened))
	handle, ok := opened["correlation_ref"].(string)
	require.True(t, ok, "the response must carry a correlation_ref handle")
	require.NotEmpty(t, handle, "the response must carry an amend handle")

	// Verbatim. Any transformation here would test the test rather than the contract.
	closeBody := fmt.Sprintf(`{"type":"deployment.completed",
		"subject":"acme/prod","occurred_at":"2026-09-22T01:00:00Z",
		"correlation_ref":%q,"idempotency_key":%q}`, handle, idemKey(t, "close"))
	rec = post(t, f.srv, f.producerKey, closeBody)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var closed map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &closed))
	require.Equal(t, handle, closed["correlation_ref"],
		"the handle is stable across appends to one change")

	var changes int
	require.NoError(t, f.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM change WHERE tenant = $1`,
		f.producerTenant.String()).Scan(&changes))
	assert.Equal(t, 1, changes,
		"echoing the handle must amend the change, not fork a second one")

	var status, lastEvent string
	require.NoError(t, f.store.Pool().QueryRow(context.Background(),
		`SELECT status, last_event_id FROM change WHERE tenant = $1`,
		f.producerTenant.String()).Scan(&status, &lastEvent))
	assert.Equal(t, "completed", status,
		"the change opened and then closed, rather than being a point from the start")
	assert.Equal(t, closed["id"], lastEvent,
		"both events landed on it, and the amending one folded last")
}

// TestPostRefusesARefStartingWithTheCredentialsLocus pins the guard against the one shape
// that used to fork silently. The fixture's credential has locus "github", so a producer
// echoing the append response's "ref" — already prefixed — sends "github/dep-1", which would
// compose and store "github/github/dep-1": a second change, with nothing erroring.
//
// The assertion names the composed value rather than just the status, because a 400 for the
// wrong reason (a length or whitespace failure, say) would pass a status-only check.
func TestPostRefusesARefStartingWithTheCredentialsLocus(t *testing.T) {
	f := newIngressFixture(t)
	body := fmt.Sprintf(`{"type":"deployment.started",
		"subject":"acme/prod","occurred_at":"2026-09-22T00:00:00Z",
		"correlation_ref":"github/dep-1","idempotency_key":%q}`, idemKey(t, "1"))
	rec := post(t, f.srv, f.producerKey, body)

	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "github/github/dep-1",
		"the refusal must show what would have been stored, or the caller cannot see the bug")
	assert.Contains(t, rec.Body.String(), "correlation_ref",
		"and must name the field to echo instead of ref")
}

// TestPostAcceptsALocusPrefixedRefWhenAllowed is the escape hatch the guard needs: a producer
// whose locus is "github" and whose own refs genuinely begin "github/" is not wrong, just
// unusual, and an operator can say so.
//
// It asserts the STORED ref, not just the status. A guard lifted by silently stripping the
// prefix would also return 201, and that is the behaviour this design rejected.
func TestPostAcceptsALocusPrefixedRefWhenAllowed(t *testing.T) {
	f := newIngressFixtureWith(t, ingress.WithAllowLocusPrefixedRef(true))
	body := fmt.Sprintf(`{"type":"deployment.started",
		"subject":"acme/prod","occurred_at":"2026-09-22T00:00:00Z",
		"correlation_ref":"github/dep-1","idempotency_key":%q}`, idemKey(t, "1"))
	rec := post(t, f.srv, f.producerKey, body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "github/github/dep-1", got["ref"],
		"lifting the guard stores the composed ref; it does not strip the prefix")
}

// TestPostAcceptsASlashedRefThatIsNotLocusPrefixed keeps the guard narrow. Only the echo
// shape is refused — a path-shaped producer ref, which this domain is full of, still works
// with the guard on.
func TestPostAcceptsASlashedRefThatIsNotLocusPrefixed(t *testing.T) {
	f := newIngressFixture(t)
	body := fmt.Sprintf(`{"type":"deployment.started",
		"subject":"acme/prod","occurred_at":"2026-09-22T00:00:00Z",
		"correlation_ref":"myorg/repo#123","idempotency_key":%q}`, idemKey(t, "1"))
	rec := post(t, f.srv, f.producerKey, body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "github/myorg/repo#123", got["ref"])
}

// TestAmendHandleRoundTripsForASlashedProducerRef is the Review Focus case. ProducerRef
// cuts at the FIRST slash only, so a producer ref containing one comes back whole —
// splitting on every slash would truncate it to "repo#123" and the echo would address
// nothing.
func TestAmendHandleRoundTripsForASlashedProducerRef(t *testing.T) {
	f := newIngressFixture(t)
	body := fmt.Sprintf(`{"type":"deployment.started",
		"subject":"acme/prod","occurred_at":"2026-09-22T00:00:00Z",
		"correlation_ref":"myorg/repo#123","idempotency_key":%q}`, idemKey(t, "1"))
	rec := post(t, f.srv, f.producerKey, body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "myorg/repo#123", got["correlation_ref"],
		"only the locus is stripped; the producer's own slashes survive")
	assert.Equal(t, "github/myorg/repo#123", got["ref"],
		"and the stored ref keeps the locus the credential supplied")
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
	body := `{"type":"deployment.started","occurred_at":"2026-09-14T10:00:00Z"}`
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

	q, err := postgres.NewEventQuery(f.producerTenant, 0)
	require.NoError(t, err)
	evs, _, err := f.store.ListEvents(context.Background(), q)
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
	huge := fmt.Sprintf(`{"type":"deployment.started",
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
// pattern it registers, whether the handler it registers is wrapped in the
// authenticator's Require middleware, and — when it is — which capability, e.g.
// "CapAppend", that call names.
type routeRegistration struct {
	pattern     string
	authWrapped bool
	capability  string
}

// capabilityName reports the auth.Cap* identifier e names, e.g. "CapAppend" for the
// expression auth.CapAppend. It matches the qualified-identifier shape rather than
// evaluating the expression, which is what lets authWrapperCapability tell one
// capability from another by reading the source rather than by running it.
func capabilityName(e ast.Expr) (string, bool) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "auth" {
		return "", false
	}
	return sel.Sel.Name, true
}

// isRequireOnAuthn reports whether e is the selector s.authn.Require — the credential
// middleware's method expression, split out of authWrapperCapability so neither function
// carries both the receiver check and the call shape's complexity at once.
func isRequireOnAuthn(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Require" {
		return false
	}
	recvSel, ok := sel.X.(*ast.SelectorExpr)
	if !ok || recvSel.Sel.Name != "authn" {
		return false
	}
	recv, ok := recvSel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return recv.Name == "s"
}

// authWrapperCapability reports the capability named by e, when e is the expression
// s.authn.Require(auth.Cap...)(...) — the credential middleware every non-probe route
// must be registered through. It matches the call syntactically rather than by
// behaviour because that is the point: a route can only be checked for a credential if
// the registration puts the middleware in its path, and no HTTP-level probe of one URL
// can show that some other URL did the same, let alone which capability it required.
func authWrapperCapability(e ast.Expr) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	inner, ok := call.Fun.(*ast.CallExpr)
	if !ok || len(inner.Args) != 1 {
		return "", false
	}
	if !isRequireOnAuthn(inner.Fun) {
		return "", false
	}
	return capabilityName(inner.Args[0])
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
	capability, authWrapped := authWrapperCapability(call.Args[1])
	return routeRegistration{
		pattern: pattern, authWrapped: authWrapped, capability: capability,
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
// guards every write route ingress exposes with the append capability specifically —
// not just the one an HTTP-level test happens to send an unauthenticated request to,
// and not merely SOME capability, which Require(auth.CapRead) on a write route would
// also satisfy.
//
// TestIngressRequiresACredential drives POST /v1/events without a credential, which
// proves that one path is guarded and says nothing about any other. POST
// /v1/events/cloudevents was in exactly that position: it IS wrapped, but deleting the
// middleware from its registration compiled and broke no test in this suite. Every other
// CloudEvents test sets Authorization before sending, so none of them would ever have
// noticed, and the same would be true of a write route nobody has added yet. Reading the
// registration is what generalizes the property from "this URL requires a credential" to
// "every non-probe route requires one that can append".
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
		want := "CapAppend"
		if strings.Contains(r.pattern, " /v1/admin/") {
			want = "CapAdmin"
		}
		assert.Equal(t, want, r.capability,
			"route %q requires capability %q, not %q: a write route on the write side of "+
				"the log needs append, and an admin route needs admin",
			r.pattern, r.capability, want)
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
	body := fmt.Sprintf(`{"type":"deployment.started",
		"subject":"acme/prod","occurred_at":"2026-09-14T10:00:00Z",
		"idempotency_key":%q,"attributes":{"service.version":"2.5.0"}}`, idemKey(t, "1"))

	rec := post(t, f.srv, f.producerKey, body)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(),
		"attributes encode to 27 bytes of JSON; the limit is 26")
}

// requestLines keeps the records the request middleware wrote.
func requestLines(records []map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if r["msg"] == "request" {
			out = append(out, r)
		}
	}
	return out
}

func TestAnAppendLogsOneLineWithItsIdentifiers(t *testing.T) {
	f := newIngressFixture(t)
	records := captureLogs(t)
	key := idemKey(t, "logged")
	rec := post(t, f.srv, f.producerKey, bodyWith(fmt.Sprintf(`"idempotency_key":%q`, key)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp struct {
		Ref string `json:"ref"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	prefix, _, ok := auth.SplitKey(f.producerKey)
	require.True(t, ok)

	lines := requestLines(records())
	require.Len(t, lines, 1)
	line := lines[0]
	assert.Equal(t, "POST /v1/events", line["route"])
	assert.InDelta(t, 201, line["status"], 0)
	assert.Equal(t, key, line["idempotency_key"])
	assert.Equal(t, "acme/prod", line["subject"])
	assert.Equal(t, "t.started", line["type"])
	assert.Equal(t, resp.Ref, line["ref"])
	assert.Equal(t, f.producerTenant.String(), line["tenant"])
	assert.Equal(t, prefix, line["credential"])
}

func TestAReplayLogsTheOriginalEventsRef(t *testing.T) {
	f := newIngressFixture(t)
	body := bodyWith(fmt.Sprintf(`"idempotency_key":%q`, idemKey(t, "replay")))
	first := post(t, f.srv, f.producerKey, body)
	require.Equal(t, http.StatusCreated, first.Code)
	var resp struct {
		Ref string `json:"ref"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &resp))

	records := captureLogs(t)
	again := post(t, f.srv, f.producerKey, body)
	require.Equal(t, http.StatusOK, again.Code)
	lines := requestLines(records())
	require.Len(t, lines, 1)
	assert.Equal(t, resp.Ref, lines[0]["ref"])
	// The second request resolves its credential from resolve's cache. KeyPrefix is set on
	// resolve's single success return, after the cache lookup inside a.credential; this pins
	// that a later early return on a cache hit would not blank the field.
	prefix, _, _ := auth.SplitKey(f.producerKey)
	assert.Equal(t, prefix, lines[0]["credential"])
}

func TestACloudEventLogsItsComputedIdempotencyKey(t *testing.T) {
	f := newIngressFixture(t)
	records := captureLogs(t)
	rec := postCE(t, f.srv, f.producerKey, bodyCE(t, "acme/prod"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	lines := requestLines(records())
	require.Len(t, lines, 1)
	assert.Equal(t, "POST /v1/events/cloudevents", lines[0]["route"])
	assert.Equal(t, storedEvent(t, f.store, rec).IdempotencyKey, lines[0]["idempotency_key"])
}

func TestABadTokenLogsItsErrorAndNoCredential(t *testing.T) {
	f := newIngressFixture(t)
	records := captureLogs(t)
	rec := post(t, f.srv, "wrong", validBody)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	lines := requestLines(records())
	require.Len(t, lines, 1)
	assert.Equal(t, body.Error, lines[0]["error"])
	assert.NotContains(t, lines[0], "credential")
	assert.NotContains(t, lines[0], "tenant")
}

func TestASubjectOutsideTheScopeLogsTheScopeMessage(t *testing.T) {
	f := newIngressFixture(t)
	records := captureLogs(t)
	rec := post(t, f.srv, f.scopedKey, bodySubject("other/prod"))
	require.Equal(t, http.StatusForbidden, rec.Code)
	lines := requestLines(records())
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0]["error"], "outside this credential's scope")
	assert.Contains(t, lines[0], "credential", "the credential resolved; only its scope refused")
}

func TestAMalformedBodyLogsItsErrorAndNoEventFields(t *testing.T) {
	f := newIngressFixture(t)
	records := captureLogs(t)
	rec := post(t, f.srv, f.producerKey, `{not json`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	lines := requestLines(records())
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0]["error"], "malformed JSON body")
	for _, field := range []string{"subject", "type", "idempotency_key"} {
		assert.NotContains(t, lines[0], field)
	}
}

func TestAGlobalCredentialLogsNoTenantKey(t *testing.T) {
	f := newIngressFixture(t)
	key := seedGlobalCredential(t, f.store, "producer")
	records := captureLogs(t)
	post(t, f.srv, key, validBody)
	lines := requestLines(records())
	require.Len(t, lines, 1)
	assert.NotContains(t, lines[0], "tenant")
	assert.Contains(t, lines[0], "credential")
}

func TestOversizedFieldsAreCutAndFlagged(t *testing.T) {
	f := newIngressFixture(t)
	records := captureLogs(t)
	subject := strings.Repeat("s", 4<<10)
	key := strings.Repeat("k", 4<<10)
	rec := post(t, f.srv, f.producerKey, fmt.Sprintf(`{"type":"t.started","subject":%q,
		"occurred_at":"2026-09-22T00:00:00Z","idempotency_key":%q}`, subject, key))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	line := requestLines(records())[0]
	assert.Len(t, line["subject"], 1024)
	assert.Equal(t, true, line["subject_truncated"])
	assert.Len(t, line["idempotency_key"], 1024)
	assert.Equal(t, true, line["idempotency_key_truncated"])
}

func TestNoKeyOrAttributeValueReachesTheLog(t *testing.T) {
	f := newIngressFixture(t)
	records := captureLogs(t)
	const marker = "attr-value-that-must-not-be-logged"
	rec := post(t, f.srv, f.producerKey, bodyWith(fmt.Sprintf(`"attributes":{"x":%q}`, marker)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	all, err := json.Marshal(records())
	require.NoError(t, err)
	assert.NotContains(t, string(all), marker)
	assert.NotContains(t, string(all), f.producerKey)
	_, secret, _ := auth.SplitKey(f.producerKey)
	assert.NotContains(t, string(all), secret)
}

// TestARequestLineCarriesTheTraceIdWhenTracing pins the middleware's place inside otelhttp.
// captureLogs uses a plain JSON handler, which never adds trace ids, so this test logs
// through the daemons' own handler and installs a real tracer provider.
func TestARequestLineCarriesTheTraceIdWhenTracing(t *testing.T) {
	// The provider goes in before the fixture: Routes builds otelhttp's handler, which takes
	// its tracer from the global provider at that moment.
	tp := sdktrace.NewTracerProvider()
	previousTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(previousTP); _ = tp.Shutdown(context.Background()) })
	f := newIngressFixture(t)
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(logging.Handler(&buf, slog.LevelInfo)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	post(t, f.srv, f.producerKey, validBody)

	var line map[string]any
	dec := json.NewDecoder(&buf)
	for dec.More() {
		var r map[string]any
		require.NoError(t, dec.Decode(&r))
		if r["msg"] == "request" {
			line = r
		}
	}
	require.NotNil(t, line)
	assert.Regexp(t, `^[0-9a-f]{32}$`, line["trace_id"])
}

func TestTheProbesLogNoRequestLine(t *testing.T) {
	f := newIngressFixture(t)
	records := captureLogs(t)
	f.srv.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Empty(t, requestLines(records()))
}

// TestAReplayWithADifferentBodyLogsTheOriginalsSubject: a reused idempotency key returns the
// original event, so the line names its subject and type, not the discarded duplicate's.
func TestAReplayWithADifferentBodyLogsTheOriginalsSubject(t *testing.T) {
	f := newIngressFixture(t)
	key := idemKey(t, "replay-different")
	first := post(t, f.srv, f.producerKey, fmt.Sprintf(`{"type":"t.started","subject":"acme/prod",
		"occurred_at":"2026-09-22T00:00:00Z","idempotency_key":%q}`, key))
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())

	records := captureLogs(t)
	again := post(t, f.srv, f.producerKey, fmt.Sprintf(`{"type":"t.completed","subject":"other/x",
		"occurred_at":"2026-09-22T00:00:00Z","idempotency_key":%q}`, key))
	require.Equal(t, http.StatusOK, again.Code, again.Body.String())
	line := requestLines(records())[0]
	assert.Equal(t, "acme/prod", line["subject"])
	assert.Equal(t, "t.started", line["type"])
}

// TestAFailedPointAppendLogsNoRef: a point's ref is minted from the event id, and a rejected
// point is never stored, so a ref on its line would name a change that does not exist.
func TestAFailedPointAppendLogsNoRef(t *testing.T) {
	f := newIngressFixture(t)
	records := captureLogs(t)
	// No type: the event is built, then refused by validation inside the store's append.
	rec := post(t, f.srv, f.producerKey,
		`{"subject":"acme/prod","occurred_at":"2026-09-22T00:00:00Z"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	line := requestLines(records())[0]
	assert.NotContains(t, line, "ref")
	assert.Equal(t, "acme/prod", line["subject"])
}
