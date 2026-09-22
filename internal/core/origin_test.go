package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOriginForCredentialType(t *testing.T) {
	for _, tc := range []struct {
		credentialType string
		want           Origin
		wantErr        bool
	}{
		{"machine_key", OriginProcess, false},
		{"oidc", OriginHuman, false},
		{"", Origin(""), true},
		{"invented", Origin(""), true},
	} {
		got, err := OriginForCredentialType(tc.credentialType)
		if tc.wantErr {
			require.Error(t, err, tc.credentialType)
			continue
		}
		require.NoError(t, err, tc.credentialType)
		assert.Equal(t, tc.want, got)
	}
}
