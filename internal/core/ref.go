package core

import (
	"fmt"
	"strings"
)

// Ref is a change's identity within a tenant: a locus and the producer's own ref, which the
// store encodes as "<locus>/<producer>".
//
// It holds the STORED FORM and nothing else, deriving the two halves on demand. That is
// deliberate: a struct holding the halves separately has to decide what String() renders
// for a value whose producer half is empty, and the two answers — "ci" and "ci/" — are both
// wrong for one of the inputs RefFromRow must round-trip. Holding the string makes the
// round-trip exact by construction, and the halves are read rarely.
//
// Storing the two parts in separate columns is arguably cleaner: the producer's ref
// survives verbatim and the "/" rule disappears. It was rejected on blast radius, since it
// changes change's primary key, core.Change and Fold, and every query and render adapter,
// where the composite string touches ingest only. Nothing is lost: a locus cannot contain
// "/", so splitting on the first one recovers the producer's ref exactly, and migration 0007
// filters by locus through a column generated that way. Keeping the composition here is
// what makes revisiting it a change to the store rather than to every site that names a ref.
//
// The zero Ref means "no ref": an event with no correlation ref holds one, and it renders
// as the empty string rather than as "/".
type Ref struct{ s string }

// NewRef composes. It refuses the two shapes that degenerate — a zero locus, which yields
// "/x" with no namespace, and an empty producer ref, which yields "ci/" and names no change.
//
// It does NOT validate the producer's ref beyond emptiness. auth.ValidateRef must have run
// on the producer's form BEFORE this is called, because composing first moves a leading or
// trailing space away from the string's edge and past the check that exists to catch it.
// That ordering is a property of the caller, and
// TestPostRejectsARefWithLeadingWhitespaceEvenAfterTheLocusPrefix pins it.
func NewRef(locus Locus, producer string) (Ref, error) {
	if locus.IsZero() {
		return Ref{}, fmt.Errorf("ref %q has no locus", producer)
	}
	if producer == "" {
		return Ref{}, fmt.Errorf("ref under locus %q is empty", locus.v)
	}
	return composeRef(locus, producer), nil
}

// composeRef is the one statement of the encoding: <locus>/<producer>. NewRef and RefFor
// both build through it, so nothing else may restate the separator.
//
// It does not validate: NewRef checks locus and producer before calling it, and RefFor may
// call it directly because a point event's locus and id are already validated by the time
// Fold runs.
func composeRef(locus Locus, producer string) Ref {
	return Ref{s: locus.v + "/" + producer}
}

// ParseRef splits at the FIRST "/". A locus cannot contain one, so both halves are recovered
// exactly, and a producer ref containing further slashes survives whole: "dev/deploy/api/7"
// has locus "dev" and producer "deploy/api/7".
//
// It refuses three shapes: no "/" at all, an empty locus half, and an empty producer half.
// The last is STRICTER than change_ref_has_locus, which admits "ci/" because
// strpos(...) > 1 constrains only the locus side. Being stricter is safe because ParseRef
// reads query input, where refusing means matching nothing. Stored values go through
// RefFromRow, which does not validate, so a legally-stored "ci/" still round-trips.
func ParseRef(s string) (Ref, error) {
	locus, producer, found := strings.Cut(s, "/")
	if !found {
		return Ref{}, fmt.Errorf("ref %q has no locus: expected <locus>/<ref>", s)
	}
	l, err := NewLocus(locus)
	if err != nil {
		return Ref{}, fmt.Errorf("ref %q: %w", s, err)
	}
	return NewRef(l, producer)
}

// RefFromRow is for values already stored, where the CHECK constraints settled the shape at
// write time. It does not validate, for the same reason LocusFromRow and TenantIDFromRow do
// not: a stored value's validity was settled when it was written, and failing a read on it
// would make a row unreadable rather than unwritable.
func RefFromRow(s string) Ref { return Ref{s: s} }

// String renders the stored form. The zero Ref renders empty, not "/".
func (r Ref) String() string { return r.s }

// Locus is the half before the first "/". A Ref built by NewRef or ParseRef always has one;
// one built by RefFromRow from a malformed row may not, and returns the zero Locus.
func (r Ref) Locus() Locus {
	l, _, found := strings.Cut(r.s, "/")
	if !found {
		return Locus{}
	}
	return Locus{v: l}
}

// ProducerRef is the half after the first "/", which may itself contain slashes.
func (r Ref) ProducerRef() string {
	_, p, _ := strings.Cut(r.s, "/")
	return p
}

// IsZero reports whether this is the absent ref — what an event with no correlation ref
// carries, and what the empty string carried before.
func (r Ref) IsZero() bool { return r.s == "" }
