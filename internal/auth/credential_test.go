package auth_test

import (
	"crypto/rand"
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

func TestNewKeyIsDistinctEachTime(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		_, prefix, _, err := auth.NewKey(rand.Reader)
		require.NoError(t, err)
		require.False(t, seen[prefix], "prefix collision")
		seen[prefix] = true
	}
}
