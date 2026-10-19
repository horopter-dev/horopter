package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/core"
)

func mustLocus(t *testing.T, v string) core.Locus {
	t.Helper()
	l, err := core.NewLocus(v)
	require.NoError(t, err)
	return l
}

func TestNewRefComposesAndRoundTrips(t *testing.T) {
	r, err := core.NewRef(mustLocus(t, "ci"), "dep-7")
	require.NoError(t, err)
	assert.Equal(t, "ci/dep-7", r.String())
	assert.Equal(t, "ci", r.Locus().String())
	assert.Equal(t, "dep-7", r.ProducerRef())

	back, err := core.ParseRef(r.String())
	require.NoError(t, err)
	assert.Equal(t, r, back, "String and ParseRef must round-trip exactly")
}

// The zero Ref renders empty: rendering a slash would change every point event's egress JSON.
func TestZeroRefIsZeroAndRendersEmpty(t *testing.T) {
	var r core.Ref
	assert.True(t, r.IsZero())
	assert.Empty(t, r.String())
}

func TestNewRefRejectsAZeroLocusAndAnEmptyProducerRef(t *testing.T) {
	_, err := core.NewRef(core.Locus{}, "dep-7")
	require.Error(t, err, "a zero locus composes to /dep-7, which has no namespace")

	_, err = core.NewRef(mustLocus(t, "ci"), "")
	require.Error(t, err, "an empty producer ref composes to ci/, which names no change")
}

// Split at the first slash: the last would yield locus dev/deploy/api, which NewLocus rejects.
func TestParseRefSplitsAtTheFirstSlashOnly(t *testing.T) {
	r, err := core.ParseRef("dev/deploy/api/7")
	require.NoError(t, err)
	assert.Equal(t, "dev", r.Locus().String())
	assert.Equal(t, "deploy/api/7", r.ProducerRef())
	assert.Equal(t, "dev/deploy/api/7", r.String(), "the stored form must survive intact")
}

// ParseRef refuses "ci/", which change_ref_has_locus admits: it names no change to query.
func TestParseRefRefusesShapesTheDatabaseWouldAccept(t *testing.T) {
	for name, in := range map[string]string{
		"no slash":           "dep-7",
		"empty locus":        "/dep-7",
		"empty producer ref": "ci/",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := core.ParseRef(in)
			require.Error(t, err)
		})
	}
}

// RefFromRow round-trips "ci/": a validating constructor would make a stored row unreadable.
func TestRefFromRowDoesNotValidate(t *testing.T) {
	r := core.RefFromRow("ci/")
	assert.Equal(t, "ci/", r.String(), "a stored value must survive a read unchanged")

	bare := core.RefFromRow("01ABC")
	assert.Equal(t, "01ABC", bare.String())
}
