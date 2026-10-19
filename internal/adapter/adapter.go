// Package adapter defines the render boundary: vendor concepts live below it and nowhere else.
package adapter

import (
	"time"

	"horopter.dev/horopter/internal/core"
)

// Renderer converts neutral changes into one vendor's payload; now is passed in for golden tests.
type Renderer interface {
	Render(changes []core.Change, now time.Time) (any, error)
}
