package ingress

import (
	"fmt"
	"os"
	"strconv"
)

// DefaultMaxAttributeBytes is the attribute cap when the variable is unset.
const DefaultMaxAttributeBytes = 4096

const maxAttributeBytesVar = "HOROPTER_INGRESS_MAX_EVENT_ATTRIBUTE_BYTES"

// MaxAttributeBytesFromEnv returns the attribute cap, at most MaxBodyBytes; bad values error.
func MaxAttributeBytesFromEnv() (int, error) {
	raw := os.Getenv(maxAttributeBytesVar)
	if raw == "" {
		return DefaultMaxAttributeBytes, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > MaxBodyBytes {
		return 0, fmt.Errorf(
			"%s=%q is not a whole number of bytes from 1 to %d; set it within that range, "+
				"or unset it for the %d default",
			maxAttributeBytesVar, raw, MaxBodyBytes, DefaultMaxAttributeBytes)
	}
	return n, nil
}

const allowLocusPrefixedRefVar = "HOROPTER_INGRESS_ALLOW_LOCUS_PREFIXED_REF"

// AllowLocusPrefixedRefFromEnv defaults false: a locus-prefixed ref is usually an echoed ref.
func AllowLocusPrefixedRefFromEnv() (bool, error) {
	raw := os.Getenv(allowLocusPrefixedRefVar)
	if raw == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf(
			"%s=%q is not a boolean; use true or false, or unset it to keep the guard on",
			allowLocusPrefixedRefVar, raw)
	}
	return v, nil
}
