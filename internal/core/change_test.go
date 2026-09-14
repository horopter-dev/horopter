package core_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/yo61/horopter/internal/core"
)

func TestOpenSentinelIsFarFutureNotInfinity(t *testing.T) {
	// SQLite has no 'infinity' timestamp; a literal must sort after any real value.
	assert.Equal(t, "9999-12-31T23:59:59Z", core.OpenSentinel.Format(time.RFC3339))
}

func TestChangeShapes(t *testing.T) {
	start := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	end := start.Add(20 * time.Minute)

	point := core.Change{Start: start, End: &start, Status: core.StatusOccurred}
	open := core.Change{Start: start, End: nil, Status: core.StatusInProgress}
	closed := core.Change{Start: start, End: &end, Status: core.StatusCompleted}

	assert.True(t, point.IsPoint())
	assert.False(t, point.IsOpen())

	assert.True(t, open.IsOpen())
	assert.False(t, open.IsPoint(), "an open change must never be mistaken for a point")

	assert.False(t, closed.IsOpen())
	assert.False(t, closed.IsPoint())
}

func TestEndEffective(t *testing.T) {
	start := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	end := start.Add(20 * time.Minute)

	assert.Equal(t, end, core.Change{Start: start, End: &end}.EndEffective())
	assert.Equal(t, core.OpenSentinel, core.Change{Start: start, End: nil}.EndEffective())
}
