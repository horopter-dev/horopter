package ingress_test

import (
	"strconv"
	"testing"

	"github.com/horopter-dev/horopter/internal/ingress"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const maxAttributeBytesVar = "HOROPTER_INGRESS_MAX_EVENT_ATTRIBUTE_BYTES"

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
