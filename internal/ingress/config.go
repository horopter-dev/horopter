package ingress

import (
	"fmt"
	"os"
	"strconv"
)

// DefaultMaxAttributeBytes is the largest attribute map, in bytes of JSON, horopter-ingress
// accepts when HOROPTER_INGRESS_MAX_EVENT_ATTRIBUTE_BYTES is unset. The docs site's Attributes
// page's recommended keys, fully populated, come to about 560 bytes, and a 40-line release note
// as the description to about 3.2 KiB; anything larger belongs behind a link.
const DefaultMaxAttributeBytes = 4096

const maxAttributeBytesVar = "HOROPTER_INGRESS_MAX_EVENT_ATTRIBUTE_BYTES"

// MaxAttributeBytesFromEnv returns the attribute cap horopter-ingress applies: the value of
// HOROPTER_INGRESS_MAX_EVENT_ATTRIBUTE_BYTES, or DefaultMaxAttributeBytes when it is unset or
// empty. A value that is not a whole number from 1 to MaxBodyBytes is an error naming the
// variable and the value, so the daemon refuses to start rather than run with a cap nobody
// chose. MaxBodyBytes is the ceiling because attributes cannot be larger than the body that
// carried them.
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

// allowLocusPrefixedRefVar turns off the guard against a correlation_ref whose first segment
// is the credential's own locus.
const allowLocusPrefixedRefVar = "HOROPTER_INGRESS_ALLOW_LOCUS_PREFIXED_REF"

// AllowLocusPrefixedRefFromEnv reports whether horopter-ingress accepts a correlation_ref
// whose first segment equals the appending credential's locus — "ci/dep-1" under locus "ci",
// which composes and stores "ci/ci/dep-1".
//
// It defaults to FALSE, because that shape is almost always a producer echoing the append
// response's "ref" where it meant "correlation_ref": ref is already locus-prefixed, so
// echoing it addresses a second change and nothing errors. Refusing it turns a silent fork
// into a 400 naming the mistake.
//
// Set it true for the producer this guard is wrong about: one whose locus is "ci" and whose
// own refs genuinely begin "ci/". The setting is per-daemon rather than per-credential, so
// turning it on lifts the guard for every producer on that ingress.
//
// A value Go's bool parser does not recognise is an error naming the variable and the value,
// so the daemon refuses to start rather than run with a guard nobody chose — the same rule
// MaxAttributeBytesFromEnv applies.
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
