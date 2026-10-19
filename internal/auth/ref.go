package auth

import (
	"fmt"
	"strings"

	"horopter.dev/horopter/internal/core"
)

// MaxRefBytes is the largest a ref may be, in bytes.
const MaxRefBytes = 512

const maxLocusBytes = 64

func validateIdentifier(kind, value string, maxBytes int) error {
	switch {
	case value == "":
		return fmt.Errorf("%s is required", kind)
	case len(value) > maxBytes:
		return fmt.Errorf("%s is %d bytes; the limit is %d", kind, len(value), maxBytes)
	case strings.ContainsRune(value, 0):
		return fmt.Errorf("%s contains a null byte, which Postgres cannot store", kind)
	case strings.TrimSpace(value) != value:
		return fmt.Errorf("%s has leading or trailing whitespace; %q and %q are distinct",
			kind, value, strings.TrimSpace(value))
	}
	return nil
}

// ValidateRef reports whether ref is an acceptable producer-chosen change identifier.
func ValidateRef(ref string) error { return validateIdentifier("ref", ref, MaxRefBytes) }

// ValidateLocus adds operator policy to core.NewLocus's structural rules for a credential's locus.
func ValidateLocus(locus string) error {
	if _, err := core.NewLocus(locus); err != nil {
		return err
	}
	if locus == "legacy" {
		return fmt.Errorf(
			"locus %q is reserved: migration 0004 prefixes every pre-existing ref with "+
				"legacy/ so a real legacy-sourced credential's writes stay distinguishable "+
				"from it; issuing one here would collide with that backfilled namespace",
			locus)
	}
	return validateIdentifier("locus", locus, maxLocusBytes)
}
