package core

import (
	"fmt"
	"strings"
)

// Locus is the namespace a producer's refs are composed into: the first half of a stored
// ref, before its first "/". Its field is unexported so no other package can mint one by
// conversion, because a locus containing "/" would make <locus>/<ref> unsplittable and
// that is the one property every ref in the system rests on.
//
// See decisions/2026-09-22-credentials-carry-source-and-locus.md, in the horopter-internal
// repository, for why refs are namespaced at all, and the design doc there for why the
// namespace is encoded into the ref string rather than stored as its own column.
type Locus struct{ v string }

// NewLocus validates the two rules the ref encoding depends on: a locus must be non-empty,
// and must not contain "/".
//
// The rules an operator is additionally held to — the reserved name "legacy", a length cap,
// no edge whitespace — live in auth.ValidateLocus, which calls this first. Those are policy
// about what may be issued; these two are what makes the encoding reversible. core has no
// business knowing that migration 0004 backfilled a reserved name.
func NewLocus(v string) (Locus, error) {
	if v == "" {
		return Locus{}, fmt.Errorf("locus is required")
	}
	if strings.Contains(v, "/") {
		return Locus{}, fmt.Errorf(
			"locus %q contains /, which makes <locus>/<ref> unsplittable", v)
	}
	return Locus{v: v}, nil
}

// LocusFromRow is for values already stored, where event_locus_shape has enforced the rules
// at write time: rebuild, migrate, and every scan. It does not re-validate, for the same
// reason TenantIDFromRow does not — a stored value's validity was settled when it was
// written, and failing a read on it would make a row unreadable rather than unwritable.
func LocusFromRow(v string) Locus { return Locus{v: v} }

func (l Locus) String() string { return l.v }

// IsZero reports if uninitialized.
func (l Locus) IsZero() bool { return l.v == "" }
