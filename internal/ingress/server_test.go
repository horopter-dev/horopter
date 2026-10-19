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

// The shared database is never truncated, so ids are unique per process and per call.
var (
	procToken = strconv.FormatInt(time.Now().UnixNano(), 36)
	idSeq     atomic.Uint64
)

func uniqueToken() string {
	return procToken + "-" + strconv.FormatUint(idSeq.Add(1), 36)
}

// suffix only aids tracing; it plays no part in uniqueness.
func idemKey(t *testing.T, suffix string) string {
	t.Helper()
	return uniqueToken() + "-" + t.Name() + "-" + suffix
}

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

func newIngressFixtureWith(t *testing.T, srvOpts ...ingress.Option) ingressFixture {
	t.Helper()
	return newIngressFixtureOpts(t, nil, srvOpts)
}

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
	// Monotonic: the frozen clock gives every event one ULID prefix, so order must follow appends.
	srv, err := ingress.New(s, clock, ulid.Monotonic(rand.Reader, 0), authn, srvOpts...)
	require.NoError(t, err)

	producerTenant, producerKey := seedCredential(t, s, "github", "")
	_, scopedKey := seedCredential(t, s, "github", "acme/prod")

	return ingressFixture{
		srv: srv.Routes(), store: s,
		producerKey: producerKey, producerTenant: producerTenant, scopedKey: scopedKey,
	}
}

// Differs from every source a test names, so a stored event shows which one won.
const credentialSource = "test"

// Cleanup uses testdb's pool: several tests close s's pool before the test ends.
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

func eventCountBySubject(t *testing.T, store *postgres.Store, subject string) int {
	t.Helper()
	var n int
	require.NoError(t, store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM event WHERE subject = $1`, subject).Scan(&n))
	return n
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

// Keep this SELECT complete: an omitted column is one no test can see go wrong.
type storedRow struct {
	Tenant         string
	Source         string
	IdempotencyKey string
	CorrelationRef string
	Origin         core.Origin
	Locus          string
	SchemaVersion  int
}

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

// Omits an empty subject: CloudEvents rejects a present-but-empty one as malformed.
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

// Each route stamps the tenant independently, so each gets its own test.
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

// TestPostPrefixesTheRefWithTheCredentialLocus never touches ev.Locus.
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

// The only HTTP-level test of subjectAllowed on the CloudEvents route.
func TestSubjectPatternIsEnforcedOnCloudEvents(t *testing.T) {
	f := newIngressFixture(t)
	assert.Equal(t, http.StatusCreated,
		postCE(t, f.srv, f.scopedKey, bodyCE(t, "acme/prod")).Code)
	assert.Equal(t, http.StatusForbidden,
		postCE(t, f.srv, f.scopedKey, bodyCE(t, "acme/dev")).Code)
}

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

// Composing first would hide a leading space: "github/ dep-1".
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

// The type's suffix asserts nothing, so only the explicit field can open the change.
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

	// Nothing else asserts lifecycle survives the store's read path.
	q, err := postgres.NewEventQuery(f.producerTenant, 0)
	require.NoError(t, err)
	evs, _, err := f.store.ListEvents(context.Background(), q)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, core.LifecycleStarted, evs[0].Lifecycle,
		"the stored lifecycle must round-trip through the store's own read path")
}

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

// Echoing the stored ref instead would prefix it twice and open a second change.
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

// Assert the composed value: a 400 for another reason would pass a status check.
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

// Assert the stored ref: silently stripping the prefix would also return 201.
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

// ProducerRef cuts at the first slash only.
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

// Skip AppendEvent's span: it carries horopter.tenant too and would mask a dead guard.
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

// Runs without a database: the only liveness coverage on a DB-less CI leg.
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

// An id-less envelope still mints a non-empty key, so later ones would be deduped away.
func TestCloudEventMissingIDReturns400(t *testing.T) {
	f := newIngressFixture(t)
	body := `{"specversion":"1.0","source":"/buildkite/pipelines/api",
		"type":"deployment.started","subject":"acme/prod","time":"2026-09-14T10:00:00Z",
		"datacontenttype":"application/json",
		"data":{"correlation_ref":"dep-7"}}`
	rec := postCE(t, f.srv, f.producerKey, body)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	// "id" alone matches inside "invalid"; pin the SDK's field-level wording.
	assert.Contains(t, rec.Body.String(), "id: MUST",
		"the response must name the missing required field")
}

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

// Producers never retry a 4xx, so a store failure must be a 503.
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

func TestNewRejectsANilAuthenticator(t *testing.T) {
	clock := func() time.Time { return fixed }
	srv, err := ingress.New(nil, clock, rand.Reader, nil)
	require.Error(t, err, "a nil authenticator would panic on the first request rather "+
		"than reject it")
	assert.Nil(t, srv, "a rejected configuration must not yield a usable server")
}

// 413, not 400: the body may be well formed and merely too large.
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

var unauthenticatedProbeRoutes = []string{
	"GET /healthz",
	"GET /readyz",
}

type routeRegistration struct {
	pattern     string
	authWrapped bool
	capability  string
}

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

// Require(auth.CapRead) on a write route would satisfy "some capability"; check CapAppend.
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

func getUnauthenticated(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestReadyzReportsReadyWithLiveDatabase(t *testing.T) {
	f := newIngressFixture(t)
	rec := getUnauthenticated(t, f.srv, "/readyz")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// pgxpool.Close is idempotent, so the fixture's own cleanup close stays safe.
func TestReadyzReports503WhenTheDatabaseIsUnreachable(t *testing.T) {
	f := newIngressFixture(t)
	f.store.Close()

	rec := getUnauthenticated(t, f.srv, "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "database unreachable",
		"an operator reading the probe's body must be told what is wrong")
}

// Copied from egress. Safe only while no test here calls t.Parallel.
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

// pgx's error names the DB user, name, host and port; the probe is unauthenticated.
func TestReadyzDoesNotRelayTheDatabaseError(t *testing.T) {
	f := newIngressFixture(t)
	f.store.Close()

	rec := getUnauthenticated(t, f.srv, "/readyz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "closed pool",
		"the database error must not reach an unauthenticated caller")
}

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
	// KeyPrefix must survive a credential cache hit.
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

// captureLogs' plain handler never adds trace ids, so this logs through the daemons' own.
func TestARequestLineCarriesTheTraceIdWhenTracing(t *testing.T) {
	// Install the provider first: Routes takes otelhttp's tracer from it at build time.
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

// A rejected point is never stored, so a ref on its line would name nothing.
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
