package ingress_test

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/ingress"
)

const maxAttributeBytesVar = "HOROPTER_INGRESS_MAX_EVENT_ATTRIBUTE_BYTES"

const allowLocusPrefixedRefVar = "HOROPTER_INGRESS_ALLOW_LOCUS_PREFIXED_REF"

// Unset must mean guard on; handler tests set it by option, so only this sees the default.
func TestAllowLocusPrefixedRefFromEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"false", false},
		{"0", false},
		{"true", true},
		{"1", true},
	} {
		t.Run("accepts "+strconv.Quote(tc.value), func(t *testing.T) {
			t.Setenv(allowLocusPrefixedRefVar, tc.value)
			got, err := ingress.AllowLocusPrefixedRefFromEnv()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	for _, bad := range []string{"yes", "on", "maybe", "2"} {
		t.Run("refuses "+strconv.Quote(bad), func(t *testing.T) {
			t.Setenv(allowLocusPrefixedRefVar, bad)
			_, err := ingress.AllowLocusPrefixedRefFromEnv()
			require.Error(t, err)
			assert.Contains(t, err.Error(), allowLocusPrefixedRefVar,
				"the error must name the variable an operator has to fix")
			assert.Contains(t, err.Error(), bad, "and the value they set")
		})
	}
}

func TestMaxAttributeBytesFromEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{"", 4096},
		{"1", 1},
		{"8192", 8192},
		{"1048576", 1048576},
	} {
		t.Run("accepts "+strconv.Quote(tc.value), func(t *testing.T) {
			t.Setenv(maxAttributeBytesVar, tc.value)
			got, err := ingress.MaxAttributeBytesFromEnv()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	for _, value := range []string{"4k", "0", "-1", "1048577", "4096.5"} {
		t.Run("refuses "+strconv.Quote(value), func(t *testing.T) {
			t.Setenv(maxAttributeBytesVar, value)
			_, err := ingress.MaxAttributeBytesFromEnv()
			require.Error(t, err)
			assert.Contains(t, err.Error(), maxAttributeBytesVar+"="+strconv.Quote(value),
				"the error must name the variable and the value an operator has to fix")
		})
	}
}
