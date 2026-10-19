package auth_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/auth"
)

func TestTheNullableTenantIsMappedStructurally(t *testing.T) {
	t.Run("null is global", func(t *testing.T) {
		got, err := auth.TenantFrom(nil)
		require.NoError(t, err)
		assert.True(t, got.IsZero())
	})

	t.Run("a named tenant is not global", func(t *testing.T) {
		got, err := auth.TenantFrom(ptr("tnt_acme"))
		require.NoError(t, err)
		assert.Equal(t, "tnt_acme", got.String())
		assert.False(t, got.IsZero())
	})

	t.Run("an empty non-null id is refused", func(t *testing.T) {
		_, err := auth.TenantFrom(ptr(""))
		assert.Error(t, err,
			"reading it as global would widen the credential's scope, not narrow it")
	})
}
