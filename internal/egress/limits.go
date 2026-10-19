package egress

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Limits are the capacities egress admits reads against.
type Limits struct {
	// ReadMemoryBytes is the pool every admitted read reserves its estimated memory from.
	ReadMemoryBytes int64
	// AdmissionWait is how long a read waits at each gate before it is shed.
	AdmissionWait time.Duration
	// DBWaitThreshold is the database connection wait above which a read counts as overloaded.
	DBWaitThreshold time.Duration
}

const (
	readMemoryBytesVar = "HOROPTER_EGRESS_READ_MEMORY_BYTES"
	admissionWaitVar   = "HOROPTER_EGRESS_ADMISSION_WAIT"
	dbWaitThresholdVar = "HOROPTER_EGRESS_DB_WAIT_THRESHOLD"

	// minReadMemoryBytes is the smallest pool accepted, 1 MiB.
	minReadMemoryBytes = 1 << 20

	// maxAdmissionWait keeps both gates' waits within 20s of the 30s WriteTimeout.
	maxAdmissionWait = 10 * time.Second
)

// DefaultLimits returns the limits egress runs with when none of its variables is set.
func DefaultLimits() Limits {
	return Limits{
		ReadMemoryBytes: 256 << 20,
		AdmissionWait:   500 * time.Millisecond,
		DBWaitThreshold: 100 * time.Millisecond,
	}
}

// LimitsFromEnv applies the HOROPTER_EGRESS_* variables over DefaultLimits; bad values error.
func LimitsFromEnv() (Limits, error) {
	defaults := DefaultLimits()
	memory, err := readMemoryBytesFromEnv(defaults.ReadMemoryBytes)
	if err != nil {
		return Limits{}, err
	}
	wait, err := durationFromEnv(admissionWaitVar, defaults.AdmissionWait, maxAdmissionWait)
	if err != nil {
		return Limits{}, err
	}
	threshold, err := durationFromEnv(dbWaitThresholdVar, defaults.DBWaitThreshold, 0)
	if err != nil {
		return Limits{}, err
	}
	return Limits{ReadMemoryBytes: memory, AdmissionWait: wait, DBWaitThreshold: threshold}, nil
}

func readMemoryBytesFromEnv(fallback int64) (int64, error) {
	raw := os.Getenv(readMemoryBytesVar)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a whole number of bytes; set it to at least %d "+
			"(1 MiB), or unset it for the %d (256 MiB) default",
			readMemoryBytesVar, raw, minReadMemoryBytes, fallback)
	}
	if n < minReadMemoryBytes {
		return 0, fmt.Errorf("%s=%q is below the 1 MiB minimum; set it to at least %d, or "+
			"unset it for the %d (256 MiB) default",
			readMemoryBytesVar, raw, minReadMemoryBytes, fallback)
	}
	return n, nil
}

func durationFromEnv(name string, fallback, ceiling time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err == nil && d > 0 && (ceiling == 0 || d <= ceiling) {
		return d, nil
	}
	bound := "above 0"
	if ceiling > 0 {
		bound = "above 0 and at most " + ceiling.String()
	}
	return 0, fmt.Errorf("%s=%q is not a duration %s, such as %q; set one, or unset it for "+
		"the %s default", name, raw, bound, fallback.String(), fallback)
}
