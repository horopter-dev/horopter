package auth_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestConcurrentResolvesShareOneCache(t *testing.T) {
	lookup := &fakeLookup{cred: validCredential()}
	a := auth.New(lookup, time.Now, auth.Options{
		PositiveTTL: auth.DefaultPositiveTTL,
		NegativeTTL: auth.DefaultNegativeTTL,
	})
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
