package core

import (
	"fmt"
	"strings"
)

// Ref is a change's identity within a tenant, held in its stored form <locus>/<producer>.
type Ref struct{ s string }

// NewRef composes a ref; callers must run auth.ValidateRef on producer first.
func NewRef(locus Locus, producer string) (Ref, error) {
	if locus.IsZero() {
		return Ref{}, fmt.Errorf("ref %q has no locus", producer)
	}
	if producer == "" {
		return Ref{}, fmt.Errorf("ref under locus %q is empty", locus.v)
	}
	return composeRef(locus, producer), nil
}

// composeRef is the one statement of the encoding; it does not validate.
func composeRef(locus Locus, producer string) Ref {
	return Ref{s: locus.v + "/" + producer}
}

// ParseRef splits query input at the first slash, refusing an empty locus or producer half.
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

// RefFromRow wraps a stored value without validating it, so a stored row stays readable.
func RefFromRow(s string) Ref { return Ref{s: s} }

// String renders the stored form. The zero Ref renders empty, not "/".
func (r Ref) String() string { return r.s }

// Locus is the half before the first slash, or zero for a malformed stored row.
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

// IsZero reports whether this is the absent ref an uncorrelated event carries.
func (r Ref) IsZero() bool { return r.s == "" }
