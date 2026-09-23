package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"horopter.dev/horopter/internal/core"
)

func TestTenantIDZeroValueIsDistinguishable(t *testing.T) {
	var zero core.TenantID
	assert.True(t, zero.IsZero())
	assert.False(t, core.TenantIDFromRow("tnt_abc").IsZero())
}

func TestTenantIDCarriesItsValue(t *testing.T) {
	assert.Equal(t, "tnt_abc", core.TenantIDFromCredential("tnt_abc").String())
}
