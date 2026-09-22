package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/core"
)

func TestOriginForCredentialType(t *testing.T) {
	for _, tc := range []struct {
		credentialType string
		want           core.Origin
		wantErr        bool
	}{
		{"machine_key", core.OriginProcess, false},
		{"oidc", core.OriginHuman, false},
		{"", core.Origin(""), true},
		{"invented", core.Origin(""), true},
	} {
		got, err := core.OriginForCredentialType(tc.credentialType)
		if tc.wantErr {
			require.Error(t, err, tc.credentialType)
			continue
		}
		require.NoError(t, err, tc.credentialType)
		assert.Equal(t, tc.want, got)
	}
}
