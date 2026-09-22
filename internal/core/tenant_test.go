package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTenantIDZeroValueIsDistinguishable(t *testing.T) {
	var zero TenantID
	assert.True(t, zero.IsZero())
	assert.False(t, TenantIDFromRow("tnt_abc").IsZero())
}

func TestTenantIDCarriesItsValue(t *testing.T) {
	assert.Equal(t, "tnt_abc", TenantIDFromCredential("tnt_abc").String())
}
