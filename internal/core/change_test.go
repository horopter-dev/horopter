package core_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/yo61/horopter/internal/core"
)

func TestOpenSentinelIsFarFutureNotInfinity(t *testing.T) {
	// Go's time.Time cannot represent infinity; a literal must sort after any real value.
	assert.Equal(t, "9999-12-31T23:59:59Z", core.OpenSentinel.Format(time.RFC3339))
}

func TestChangeShapes(t *testing.T) {
	start := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	end := start.Add(20 * time.Minute)

	point := core.Change{Start: start, End: &start, Status: core.StatusOccurred}
	open := core.Change{Start: start, End: nil, Status: core.StatusInProgress}
	closed := core.Change{Start: start, End: &end, Status: core.StatusCompleted}
	// A same-second start/complete pair: End equals Start exactly as a point's does,
	// which is why status and not timestamp equality is what identifies a point.
	zeroDuration := core.Change{Start: start, End: &start, Status: core.StatusCompleted}

	assert.False(t, point.IsOpen())
	assert.True(t, open.IsOpen())
	assert.False(t, closed.IsOpen())
	assert.False(t, zeroDuration.IsOpen())

	assert.Equal(t, core.StatusOccurred, point.Status, "only a point carries StatusOccurred")
	assert.NotEqual(t, core.StatusOccurred, open.Status,
		"an open change must never be mistaken for a point")
	assert.NotEqual(t, core.StatusOccurred, zeroDuration.Status,
		"a zero-duration closed interval has End == Start and is still not a point")
}
