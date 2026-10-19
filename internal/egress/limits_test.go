package egress_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/egress"
)

const (
	readMemoryBytesVar = "HOROPTER_EGRESS_READ_MEMORY_BYTES"
	admissionWaitVar   = "HOROPTER_EGRESS_ADMISSION_WAIT"
	dbWaitThresholdVar = "HOROPTER_EGRESS_DB_WAIT_THRESHOLD"
)

// Sets every variable, empty when absent, so nothing leaks in from the shell.
func setLimitsEnv(t *testing.T, values map[string]string) {
	t.Helper()
	for _, name := range []string{readMemoryBytesVar, admissionWaitVar, dbWaitThresholdVar} {
		t.Setenv(name, values[name])
	}
}

func TestLimitsFromEnvDefaults(t *testing.T) {
	setLimitsEnv(t, nil)

	got, err := egress.LimitsFromEnv()

	require.NoError(t, err)
	assert.Equal(t, egress.Limits{
		ReadMemoryBytes: 268435456,
		AdmissionWait:   500 * time.Millisecond,
		DBWaitThreshold: 100 * time.Millisecond,
	}, got)
	assert.Equal(t, egress.DefaultLimits(), got)
}

func TestLimitsFromEnvOverrides(t *testing.T) {
	setLimitsEnv(t, map[string]string{
		readMemoryBytesVar: "1048576",
		admissionWaitVar:   "10s",
		dbWaitThresholdVar: "250ms",
	})

	got, err := egress.LimitsFromEnv()

	require.NoError(t, err)
	assert.Equal(t, egress.Limits{
		ReadMemoryBytes: 1 << 20,
		AdmissionWait:   10 * time.Second,
		DBWaitThreshold: 250 * time.Millisecond,
	}, got, "the smallest pool and the longest wait are both accepted")
}

func TestLimitsFromEnvRefusesInvalidValues(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{readMemoryBytesVar, "256M"},
		{readMemoryBytesVar, "0"},
		{readMemoryBytesVar, "-1"},
		{readMemoryBytesVar, "1048575"},
		{admissionWaitVar, "soon"},
		{admissionWaitVar, "0s"},
		{admissionWaitVar, "-1s"},
		{admissionWaitVar, "10001ms"},
		{dbWaitThresholdVar, "fast"},
		{dbWaitThresholdVar, "0s"},
		{dbWaitThresholdVar, "-5ms"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			setLimitsEnv(t, map[string]string{tc.name: tc.value})

			_, err := egress.LimitsFromEnv()

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.name+"="+strconv.Quote(tc.value),
				"the error must name the variable and the value an operator has to fix")
		})
	}
}

func TestLimitsFromEnvSaysWhatWouldBeAccepted(t *testing.T) {
	setLimitsEnv(t, map[string]string{readMemoryBytesVar: "256M"})

	_, err := egress.LimitsFromEnv()

	require.Error(t, err)
	assert.Equal(t, `HOROPTER_EGRESS_READ_MEMORY_BYTES="256M" is not a whole number of bytes; `+
		`set it to at least 1048576 (1 MiB), or unset it for the 268435456 (256 MiB) default`,
		err.Error())
}
