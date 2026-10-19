package core

import (
	"fmt"
	"strings"
)

// Locus is the namespace a producer's refs are composed into, before a ref's first slash.
type Locus struct{ v string }

// NewLocus holds the two rules the ref encoding needs: non-empty, and no slash.
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

// LocusFromRow does not validate: a stored row or issued credential was checked when written.
func LocusFromRow(v string) Locus { return Locus{v: v} }

func (l Locus) String() string { return l.v }

// IsZero reports if uninitialized.
func (l Locus) IsZero() bool { return l.v == "" }
