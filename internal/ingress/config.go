package ingress

import (
	"fmt"
	"os"
	"strconv"
)

// DefaultMaxAttributeBytes is the largest attribute map, in bytes of JSON, horopter-ingress
// accepts when HOROPTER_INGRESS_MAX_EVENT_ATTRIBUTE_BYTES is unset. The README's recommended
// attribute keys, fully populated, come to about 560 bytes, and a 40-line release note as the
// description to about 3.2 KiB; anything larger belongs behind a link.
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
