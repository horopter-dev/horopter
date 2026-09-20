// Package adapter defines the render boundary. Every vendor concept — timestamp
// units, tag flattening, field names — lives below this line and nowhere else.
package adapter

import (
	"time"

	"github.com/horopter-dev/horopter/internal/core"
)

// Renderer converts neutral changes into one vendor's payload.
//
// now is passed in rather than read from the clock so that rendering an open
// change stays deterministic and golden-testable.
type Renderer interface {
	Render(changes []core.Change, now time.Time) (any, error)
}
