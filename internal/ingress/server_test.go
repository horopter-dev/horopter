package ingress_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
