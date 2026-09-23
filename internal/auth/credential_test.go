package auth_test

import (
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/auth"
)

func TestNewKeyRoundTrips(t *testing.T) {
	key, prefix, hash, err := auth.NewKey(rand.Reader)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(key, "horo_"))

	gotPrefix, secret, ok := auth.SplitKey(key)
	require.True(t, ok)
	assert.Equal(t, prefix, gotPrefix)
	assert.True(t, auth.VerifySecret(secret, hash))
	assert.False(t, auth.VerifySecret(secret+"x", hash))
}

func TestSplitKeyRejectsMalformed(t *testing.T) {
	for _, in := range []string{"", "horo_", "horo_onlyprefix", "nothoro_a_b", "horo__secret"} {
		_, _, ok := auth.SplitKey(in)
		assert.False(t, ok, in)
	}
}

// TestSplitKeyRejectsAPrefixNewKeyCouldNotHaveIssued closes an unauthenticated
// memory-exhaustion vector: the prefix becomes a cache key, so a caller who can choose
// its length chooses how many bytes each cached entry costs.
func TestSplitKeyRejectsAPrefixNewKeyCouldNotHaveIssued(t *testing.T) {
	for name, in := range map[string]string{
		"too short":       "horo_abc_secret",
		"one over":        "horo_0123456789abc_secret",
		"one under":       "horo_0123456789a_secret",
		"not hex":         "horo_zzzzzzzzzzzz_secret",
		"a megabyte":      "horo_" + strings.Repeat("a", 1<<20) + "_secret",
		"uppercase brand": "HORO_0123456789ab_secret",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, ok := auth.SplitKey(in)
			assert.False(t, ok)
		})
	}
}

func TestSplitKeyAcceptsWhatNewKeyIssues(t *testing.T) {
	key, prefix, _, err := auth.NewKey(rand.Reader)
	require.NoError(t, err)
	require.Len(t, prefix, 12, "the lookup prefix is six bytes of hex")

	got, secret, ok := auth.SplitKey(key)
	require.True(t, ok)
	assert.Equal(t, prefix, got)
	assert.Len(t, secret, 64)
}

// TestHashSecretMatchesWhatNewKeyStores pins HashSecret to NewKey's own hash, which is
// what lets horoctl's --key path (an operator-supplied secret NewKey never generated)
// store a hash VerifySecret will later accept, without reimplementing the algorithm.
func TestHashSecretMatchesWhatNewKeyStores(t *testing.T) {
	key, _, hash, err := auth.NewKey(rand.Reader)
	require.NoError(t, err)

	_, secret, ok := auth.SplitKey(key)
	require.True(t, ok)
	assert.Equal(t, hash, auth.HashSecret(secret), "same secret must hash identically")
}

// TestHashSecretVerifies is the round trip HashSecret exists for: a secret this package
// never generated must still verify against the hash HashSecret produces for it.
func TestHashSecretVerifies(t *testing.T) {
	secret := "an-operator-chosen-development-secret"
	hash := auth.HashSecret(secret)
	assert.True(t, auth.VerifySecret(secret, hash))
	assert.False(t, auth.VerifySecret(secret+"x", hash))
}

func TestVerifySecretUsesAConstantTimeCompare(t *testing.T) {
	src, err := os.ReadFile("credential.go")
	require.NoError(t, err)
	assert.Contains(t, string(src), "subtle.ConstantTimeCompare",
		"a == comparison here is a timing oracle and passes every functional test")
}

func TestNewKeyIsDistinctEachTime(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		_, prefix, _, err := auth.NewKey(rand.Reader)
		require.NoError(t, err)
		require.False(t, seen[prefix], "prefix collision")
		seen[prefix] = true
	}
}
