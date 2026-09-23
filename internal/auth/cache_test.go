package auth_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/auth"
)

func TestRevocationTakesEffectWithinTheTTL(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	lookup := &fakeLookup{cred: validCredential()}
	a := auth.New(lookup, clock.Now, auth.Options{PositiveTTL: 30 * time.Second})

	assert.Equal(t, http.StatusOK, serve(t, a, auth.CapRead, "Bearer "+validKey).Code)

	lookup.revoke()
	assert.Equal(t, http.StatusOK, serve(t, a, auth.CapRead, "Bearer "+validKey).Code,
		"within the TTL the cached credential is still honoured; this is the accepted trade")

	clock.advance(31 * time.Second)
	assert.Equal(t, http.StatusUnauthorized, serve(t, a, auth.CapRead, "Bearer "+validKey).Code)
}

func TestNegativeCacheStopsRepeatedLookups(t *testing.T) {
	lookup := &fakeLookup{}
	a := auth.New(lookup, time.Now, auth.Options{NegativeTTL: 5 * time.Second})
	for range 10 {
		serve(t, a, auth.CapRead, "horo_ffffffffffff_x")
	}
	assert.Equal(t, 1, lookup.callCount(),
		"an unknown prefix must not reach the store once per request")
}

// TestAnOversizedPrefixNeverReachesTheStoreOrTheCache is the other half of the key-shape
// fix: a prefix the service could not have issued is refused before it can be retained,
// so a loop of them costs neither a query nor a cache entry.
func TestAnOversizedPrefixNeverReachesTheStoreOrTheCache(t *testing.T) {
	lookup := &fakeLookup{cred: validCredential()}
	a := auth.New(lookup, time.Now, auth.DefaultOptions())
	huge := "horo_" + strings.Repeat("a", 1<<20) + "_x"

	for range 10 {
		assert.Equal(t, http.StatusUnauthorized, serve(t, a, auth.CapRead, huge).Code)
	}
	assert.Equal(t, 0, lookup.callCount(), "a malformed key is refused before the store")

	// The cache is untouched, so the credential's own lookup is still the first one.
	require.Equal(t, http.StatusOK, serve(t, a, auth.CapRead, validKey).Code)
	assert.Equal(t, 1, lookup.callCount())
}

func TestDefaultOptionsCacheBothOutcomes(t *testing.T) {
	assert.Equal(t, 30*time.Second, auth.DefaultOptions().PositiveTTL)
	assert.Equal(t, 5*time.Second, auth.DefaultOptions().NegativeTTL)

	lookup := &fakeLookup{cred: validCredential()}
	a := auth.New(lookup, time.Now, auth.DefaultOptions())
	for range 4 {
		serve(t, a, auth.CapRead, validKey)
		serve(t, a, auth.CapRead, "horo_ffffffffffff_x")
	}
	assert.Equal(t, 2, lookup.callCount(), "one lookup for the credential, one for the miss")
}

func TestPositiveCacheStopsRepeatedLookups(t *testing.T) {
	lookup := &fakeLookup{cred: validCredential()}
	a := auth.New(lookup, time.Now, auth.Options{PositiveTTL: 30 * time.Second})
	for range 10 {
		require.Equal(t, http.StatusOK, serve(t, a, auth.CapRead, validKey).Code)
	}
	assert.Equal(t, 1, lookup.callCount())
}

func TestAWrongSecretIsVerifiedAgainstEveryCachedHit(t *testing.T) {
	lookup := &fakeLookup{cred: validCredential()}
	a := auth.New(lookup, time.Now, auth.Options{PositiveTTL: 30 * time.Second})

	require.Equal(t, http.StatusOK, serve(t, a, auth.CapRead, validKey).Code)
	assert.Equal(t, http.StatusUnauthorized,
		serve(t, a, auth.CapRead, "horo_"+knownPrefix+"_wrong").Code,
		"the cache holds the credential, never a decision about a presented secret")
}

func TestATTLOfZeroDisablesCaching(t *testing.T) {
	for name, tc := range map[string]struct {
		opts   auth.Options
		key    string
		wanted int
	}{
		"positive": {auth.Options{PositiveTTL: 0}, validKey, 3},
		"negative": {auth.Options{NegativeTTL: 0}, "horo_ffffffffffff_x", 3},
	} {
		t.Run(name, func(t *testing.T) {
			lookup := &fakeLookup{cred: validCredential()}
			a := auth.New(lookup, time.Now, tc.opts)
			for range 3 {
				serve(t, a, auth.CapRead, tc.key)
			}
			assert.Equal(t, tc.wanted, lookup.callCount())
		})
	}
}

// TestTheCacheIsBoundedByItsGenerations pins the memory bound: a flood of distinct
// unknown prefixes writes a negative entry each, so entries must be dropped rather than
// held until their TTL expires.
func TestTheCacheIsBoundedByItsGenerations(t *testing.T) {
	lookup := &fakeLookup{}
	a := auth.New(lookup, time.Now, auth.Options{NegativeTTL: time.Hour})
	flood := 3 * auth.MaxCacheEntries
	for i := range flood {
		serve(t, a, auth.CapRead, fmt.Sprintf("horo_%012x_x", i))
	}
	require.Equal(t, flood, lookup.callCount())

	serve(t, a, auth.CapRead, fmt.Sprintf("horo_%012x_x", 0))
	assert.Equal(t, flood+1, lookup.callCount(),
		"the first prefix of the flood must have been dropped, not retained for its TTL")
}

// TestAColdBurstOnOnePrefixCostsOneStoreCall is the pool-protection property: auth runs
// outside admission and admission is sized from the same pool, so concurrent resolutions
// of one cold prefix must collapse into a single query rather than take a connection each.
func TestAColdBurstOnOnePrefixCostsOneStoreCall(t *testing.T) {
	gate := make(chan struct{})
	lookup := &fakeLookup{cred: validCredential(), gate: gate}
	a := auth.New(lookup, time.Now, auth.DefaultOptions())
	handler := a.Require(auth.CapRead)(okHandler())

	codes := make([]int, 64)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+validKey)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}()
	}

	// Hold the store until the burst has piled up behind the one call that reached it.
	require.Eventually(t, func() bool { return lookup.callCount() >= 1 },
		5*time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	assert.Equal(t, 1, lookup.callCount(),
		"64 concurrent requests for one prefix must take one connection, not 64")
	for i, code := range codes {
		require.Equal(t, http.StatusOK, code, "request %d", i)
	}
}

// TestAWedgedStoreTimesOutAsInfrastructure covers the bound on the shared call. The
// waiters must be released by the timeout rather than held for as long as the store is
// wedged, and a timeout is the store's failure, not the credential's: classifying it as a
// credential failure would answer 401 here and 503 for a store that refused the
// connection, which is the oracle from the other direction.
func TestAWedgedStoreTimesOutAsInfrastructure(t *testing.T) {
	gate := make(chan struct{})
	lookup := &fakeLookup{cred: validCredential(), gate: gate}
	a := auth.New(lookup, time.Now, auth.Options{
		PositiveTTL:   auth.DefaultPositiveTTL,
		LookupTimeout: 20 * time.Millisecond,
	})
	handler := a.Require(auth.CapRead)(okHandler())

	recs := make([]*httptest.ResponseRecorder, 8)
	var wg sync.WaitGroup
	for i := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+validKey)
			recs[i] = httptest.NewRecorder()
			handler.ServeHTTP(recs[i], req)
		}()
	}

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the waiters outlived the lookup timeout; the shared call is unbounded")
	}

	for i, rec := range recs {
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, "request %d", i)
		assert.Empty(t, rec.Header().Get("WWW-Authenticate"),
			"a timeout is not a credential the caller can fix")
	}
	assert.Equal(t, 1, lookup.callCount(), "the burst still costs one store call")

	close(gate)
	assert.Equal(t, http.StatusOK, serve(t, a, auth.CapRead, validKey).Code,
		"a timeout is a store failure, so it is not cached")
	assert.Equal(t, 2, lookup.callCount())
}

func TestAZeroLookupTimeoutMeansTheDefaultNotUnbounded(t *testing.T) {
	assert.Equal(t, 2*time.Second, auth.DefaultLookupTimeout)
	assert.Equal(t, auth.DefaultLookupTimeout, auth.DefaultOptions().LookupTimeout)
	assert.Equal(t, auth.DefaultLookupTimeout,
		auth.LookupTimeoutOf(auth.New(&fakeLookup{}, time.Now, auth.Options{})),
		"an omitted timeout must not leave the store call unbounded")
	assert.Equal(t, 50*time.Millisecond, auth.LookupTimeoutOf(
		auth.New(&fakeLookup{}, time.Now, auth.Options{LookupTimeout: 50 * time.Millisecond})))
}

func TestConcurrentResolvesOfManyPrefixesAreSafe(t *testing.T) {
	lookup := &fakeLookup{cred: validCredential()}
	a := auth.New(lookup, time.Now, auth.DefaultOptions())
	handler := a.Require(auth.CapRead)(okHandler())

	codes := make([]int, 64)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := validKey
			if i%2 == 1 {
				key = fmt.Sprintf("horo_%012x_x", i)
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+key)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}()
	}
	wg.Wait()

	for i, code := range codes {
		want := http.StatusOK
		if i%2 == 1 {
			want = http.StatusUnauthorized
		}
		require.Equal(t, want, code, "request %d", i)
	}
}

func BenchmarkResolveCached(b *testing.B)   { benchmarkResolve(b, 30*time.Second) }
func BenchmarkResolveUncached(b *testing.B) { benchmarkResolve(b, 0) }

// benchmarkResolve measures the middleware, not the database: the lookup it misses into
// is an in-memory fake, so the uncached figure understates the real cost of a miss by a
// query and a round trip.
func benchmarkResolve(b *testing.B, ttl time.Duration) {
	a := auth.New(&fakeLookup{cred: validCredential()}, time.Now,
		auth.Options{PositiveTTL: ttl})
	handler := a.Require(auth.CapRead)(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+validKey)

	b.ReportAllocs()
	for b.Loop() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status %d", rec.Code)
		}
	}
}
