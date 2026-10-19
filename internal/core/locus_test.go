package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/core"
)

func TestNewLocusAcceptsAPlainName(t *testing.T) {
	l, err := core.NewLocus("github")
	require.NoError(t, err)
	assert.Equal(t, "github", l.String())
	assert.False(t, l.IsZero())
}

// A slash in a locus would make <locus>/<ref> unsplittable.
func TestNewLocusRejectsASlash(t *testing.T) {
	_, err := core.NewLocus("a/b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/")
}

func TestNewLocusRejectsEmpty(t *testing.T) {
	_, err := core.NewLocus("")
	require.Error(t, err)
}

// The zero Locus is uninitialised, not valid, and must not render as one.
func TestZeroLocusIsZeroAndRendersEmpty(t *testing.T) {
	var l core.Locus
	assert.True(t, l.IsZero())
	assert.Empty(t, l.String())
}
