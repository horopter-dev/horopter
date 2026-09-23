package auth_test

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
)

// validKey, knownPrefix and knownHash are one real key generated once for the whole
// suite: the hash a credential stores can only be produced by NewKey, since the hashing
// is unexported.
var validKey, knownPrefix, knownHash = generateKey()

func generateKey() (key, prefix, hash string) {
	key, prefix, hash, err := auth.NewKey(rand.Reader)
	if err != nil {
		panic("generate the suite's key: " + err.Error())
	}
	return key, prefix, hash
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeLookup answers for knownPrefix only, and counts what reached it so a test can tell
// a cache hit from a lookup.
type fakeLookup struct {
	mu    sync.Mutex
	cred  auth.Credential
	calls int
}

func (f *fakeLookup) CredentialByPrefix(
	_ context.Context, prefix string,
) (auth.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.cred.ID == "" || prefix != knownPrefix {
		return auth.Credential{}, auth.ErrNoCredential
	}
	return f.cred, nil
}

func (f *fakeLookup) revoke() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cred.RevokedAt = ptr(time.Unix(0, 0))
}

func (f *fakeLookup) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func ptr[T any](v T) *T { return &v }

func validCredential() auth.Credential {
	return auth.Credential{
		ID: "cred_reader", PrincipalID: "prn_grafana", Type: "machine_key",
		KeyHash: knownHash, Role: "reader",
		Capabilities: []auth.Capability{auth.CapRead},
		Tenant:       core.TenantIDFromCredential("tnt_acme"), Locus: "grafana",
	}
}

func producerCredential() auth.Credential {
	c := validCredential()
	c.ID, c.Role = "cred_producer", "producer"
	c.Capabilities = []auth.Capability{auth.CapAppend}
	c.Locus = "github"
	return c
}

func testAuthenticator(
	t *testing.T, cred auth.Credential, opts ...auth.Option,
) *auth.Authenticator {
	t.Helper()
	o := auth.Options{
		PositiveTTL: auth.DefaultPositiveTTL,
		NegativeTTL: auth.DefaultNegativeTTL,
	}
	for _, apply := range opts {
		apply(&o)
	}
	return auth.New(&fakeLookup{cred: cred}, time.Now, o)
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// serve drives one request through Require. A case naming a bare key gets the Bearer
// scheme added, so a case meant to exercise a wrong secret is not rejected earlier for
// carrying no scheme at all.
func serve(
	t *testing.T, a *auth.Authenticator, c auth.Capability, header string,
) *httptest.ResponseRecorder {
	t.Helper()
	return serveTo(t, a, c, okHandler(), header)
}

func serveTo(
	t *testing.T, a *auth.Authenticator, c auth.Capability, next http.Handler, header string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if header != "" {
		if !strings.HasPrefix(header, "Bearer ") {
			header = "Bearer " + header
		}
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	a.Require(c)(next).ServeHTTP(rec, req)
	return rec
}

func TestRequireRejectsAbsentAndInvalidCredentials(t *testing.T) {
	a := testAuthenticator(t, validCredential())
	for name, header := range map[string]string{
		"absent":         "",
		"wrong secret":   "horo_" + knownPrefix + "_wrong",
		"unknown prefix": "horo_ffffffffffff_anything",
		"malformed":      "not-a-horopter-key",
	} {
		t.Run(name, func(t *testing.T) {
			rec := serve(t, a, auth.CapRead, header)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

func TestUnknownPrefixAndWrongSecretAreIndistinguishable(t *testing.T) {
	a := testAuthenticator(t, validCredential())
	unknown := serve(t, a, auth.CapRead, "horo_ffffffffffff_anything")
	wrong := serve(t, a, auth.CapRead, "horo_"+knownPrefix+"_wrong")
	assert.Equal(t, unknown.Body.String(), wrong.Body.String(),
		"a differing body is a prefix-enumeration oracle")
	assert.Equal(t, unknown.Code, wrong.Code)
}

func TestBearerWinsOverTheConfigurableHeader(t *testing.T) {
	a := testAuthenticator(t, validCredential(), auth.WithHeader("X-Horopter-Key"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+validKey)
	req.Header.Set("X-Horopter-Key", "horo_ffffffffffff_wrong")
	rec := httptest.NewRecorder()
	a.Require(auth.CapRead)(okHandler()).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "Authorization takes precedence, deterministically")
}

func TestAnUnusableAuthorizationHeaderDoesNotFallBackToTheOtherHeader(t *testing.T) {
	a := testAuthenticator(t, validCredential(), auth.WithHeader("X-Horopter-Key"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	req.Header.Set("X-Horopter-Key", validKey)
	rec := httptest.NewRecorder()
	a.Require(auth.CapRead)(okHandler()).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"precedence is by header, not by which value happens to work")
}

func TestTheConfigurableHeaderAuthenticatesWhenAuthorizationIsAbsent(t *testing.T) {
	a := testAuthenticator(t, validCredential(), auth.WithHeader("X-Horopter-Key"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Horopter-Key", validKey)
	rec := httptest.NewRecorder()
	a.Require(auth.CapRead)(okHandler()).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestTheConfigurableHeaderIsIgnoredUnlessConfigured(t *testing.T) {
	a := testAuthenticator(t, validCredential())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Horopter-Key", validKey)
	rec := httptest.NewRecorder()
	a.Require(auth.CapRead)(okHandler()).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestInsufficientCapabilityIs403(t *testing.T) {
	a := testAuthenticator(t, producerCredential())
	rec := serve(t, a, auth.CapAdmin, "Bearer "+validKey)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "producer")
}

func TestRequireRejectsBeforeReachingTheHandler(t *testing.T) {
	a := testAuthenticator(t, producerCredential())
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	for _, tc := range []struct {
		name       string
		capability auth.Capability
		header     string
	}{
		{"unauthenticated", auth.CapRead, ""},
		{"insufficient", auth.CapAdmin, validKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			serveTo(t, a, tc.capability, next, tc.header)
			assert.False(t, reached, "auth wraps admission; a rejection must not reach next")
		})
	}
}

func TestThePrincipalReachesTheHandler(t *testing.T) {
	a := testAuthenticator(t, producerCredential())
	var got auth.Principal
	var ok bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok = auth.PrincipalFrom(r.Context())
	})
	require.Equal(t, http.StatusOK, serveTo(t, a, auth.CapAppend, next, validKey).Code)

	require.True(t, ok)
	assert.Equal(t, "cred_producer", got.CredentialID)
	assert.Equal(t, "prn_grafana", got.PrincipalID)
	assert.Equal(t, "tnt_acme", got.Tenant.String())
	assert.Equal(t, "github", got.Locus)
	assert.Equal(t, core.OriginProcess, got.Origin)
	assert.True(t, got.Can(auth.CapAppend))
	assert.False(t, got.Can(auth.CapRead))
	assert.False(t, got.IsGlobal())
}

func TestPrincipalFromReportsFalseWithoutRequire(t *testing.T) {
	_, ok := auth.PrincipalFrom(context.Background())
	assert.False(t, ok)
}

func TestACredentialWithNoTenantIsGlobal(t *testing.T) {
	cred := validCredential()
	cred.Tenant, cred.Role = core.TenantID{}, "superuser"
	cred.Capabilities = []auth.Capability{
		auth.CapAppend, auth.CapRead, auth.CapAdmin, auth.CapIssue,
	}
	a := testAuthenticator(t, cred)
	var got auth.Principal
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = auth.PrincipalFrom(r.Context())
	})
	require.Equal(t, http.StatusOK, serveTo(t, a, auth.CapAdmin, next, validKey).Code)
	assert.True(t, got.IsGlobal(), "a credential with no tenant may select any")
}

func TestARevokedCredentialIsRejected(t *testing.T) {
	cred := validCredential()
	cred.RevokedAt = ptr(time.Now().Add(-time.Second))
	a := testAuthenticator(t, cred)
	assert.Equal(t, http.StatusUnauthorized, serve(t, a, auth.CapRead, validKey).Code)
}

func TestAFutureRevocationIsNotYetInEffect(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	cred := validCredential()
	cred.RevokedAt = ptr(time.Unix(10, 0))
	a := auth.New(&fakeLookup{cred: cred}, clock.Now, auth.Options{})

	assert.Equal(t, http.StatusOK, serve(t, a, auth.CapRead, validKey).Code)
	clock.advance(10 * time.Second)
	assert.Equal(t, http.StatusUnauthorized, serve(t, a, auth.CapRead, validKey).Code)
}

func TestExpiredCredentialIsRejected(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	cred := validCredential()
	cred.ExpiresAt = ptr(time.Unix(10, 0))
	a := auth.New(&fakeLookup{cred: cred}, clock.Now, auth.Options{})

	assert.Equal(t, http.StatusOK, serve(t, a, auth.CapRead, "Bearer "+validKey).Code)
	clock.advance(11 * time.Second)
	assert.Equal(t, http.StatusUnauthorized, serve(t, a, auth.CapRead, "Bearer "+validKey).Code)
}

func TestExpiryIsCheckedEvenWhileTheCredentialIsCached(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	cred := validCredential()
	cred.ExpiresAt = ptr(time.Unix(10, 0))
	lookup := &fakeLookup{cred: cred}
	a := auth.New(lookup, clock.Now, auth.Options{PositiveTTL: time.Hour})

	require.Equal(t, http.StatusOK, serve(t, a, auth.CapRead, validKey).Code)
	clock.advance(11 * time.Second)
	assert.Equal(t, http.StatusUnauthorized, serve(t, a, auth.CapRead, validKey).Code)
	assert.Equal(t, 1, lookup.callCount(),
		"expiry is in the cached row, so it needs no second lookup to take effect")
}

func TestASubjectPatternOnAnAdminGrantIsRejected(t *testing.T) {
	cred := validCredential()
	cred.Role = "tenant_admin"
	cred.Capabilities = []auth.Capability{auth.CapRead, auth.CapAdmin}
	cred.SubjectPattern = "checkout/*"
	a := testAuthenticator(t, cred)
	assert.Equal(t, http.StatusUnauthorized, serve(t, a, auth.CapRead, validKey).Code,
		"a rebuild is whole-tenant, so a narrowed admin grant is incoherent, not ignorable")
}

func TestASubjectPatternOnANonAdminGrantIsCarried(t *testing.T) {
	cred := producerCredential()
	cred.SubjectPattern = "checkout/*"
	a := testAuthenticator(t, cred)
	var got auth.Principal
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = auth.PrincipalFrom(r.Context())
	})
	require.Equal(t, http.StatusOK, serveTo(t, a, auth.CapAppend, next, validKey).Code)
	assert.Equal(t, "checkout/*", got.SubjectPattern)
}

func TestACredentialTypeWithNoOriginIsRejected(t *testing.T) {
	cred := validCredential()
	cred.Type = "smoke_signal"
	a := testAuthenticator(t, cred)
	assert.Equal(t, http.StatusUnauthorized, serve(t, a, auth.CapRead, validKey).Code)
}
