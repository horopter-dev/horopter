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

// The zero Ref stands in for "this event has no correlation ref", which is what "" meant
// before. It must render empty: a point event's correlation_ref is "" in the egress JSON,
// and a zero Ref rendering as "/" would change every point event's payload.
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

// A producer ref may contain "/" — only the locus may not. "dev/deploy/api/7" is a legal
// stored ref whose producer part is "deploy/api/7", and splitting at the LAST slash is the
// plausible wrong implementation: it would yield locus "dev/deploy/api", which NewLocus
// would then have to reject, turning a legal ref into an error.
func TestParseRefSplitsAtTheFirstSlashOnly(t *testing.T) {
	r, err := core.ParseRef("dev/deploy/api/7")
	require.NoError(t, err)
	assert.Equal(t, "dev", r.Locus().String())
	assert.Equal(t, "deploy/api/7", r.ProducerRef())
	assert.Equal(t, "dev/deploy/api/7", r.String(), "the stored form must survive intact")
}

// ParseRef is deliberately stricter than the database. change_ref_has_locus is
// strpos(ref, '/') > 1, which constrains only the locus side, so "ci/" is legally storable.
// ParseRef refuses it because a ref naming no change is not a query worth running.
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

// RefFromRow is the other half of that asymmetry and is why it is safe. A row holding "ci/"
// must round-trip: RebuildProjection reads every stored ref, and a validating constructor
// would make a legally-written row unreadable.
func TestRefFromRowDoesNotValidate(t *testing.T) {
	r := core.RefFromRow("ci/")
	assert.Equal(t, "ci/", r.String(), "a stored value must survive a read unchanged")

	bare := core.RefFromRow("01ABC")
	assert.Equal(t, "01ABC", bare.String())
}
