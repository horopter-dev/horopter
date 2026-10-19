// Package admission holds egress's two read gates; egress wires telemetry and the rest.
package admission

import (
	"context"
	"sync"
)

// Limiter is an AIMD concurrency limit that halves once per congestion episode.
type Limiter struct {
	mu        sync.Mutex
	floor     int
	ceiling   int
	limit     int
	inFlight  int
	decreases uint64
	// freed is closed and replaced whenever a permit may have become free, waking Acquire.
	freed chan struct{}
}

// Permit is one admitted read's hold on a Limiter. Release it exactly once.
type Permit struct {
	limiter   *Limiter
	decreases uint64
}

// NewLimiter returns a Limiter at initial; it requires 1 <= floor <= initial <= ceiling.
func NewLimiter(floor, initial, ceiling int) *Limiter {
	return &Limiter{floor: floor, ceiling: ceiling, limit: initial, freed: make(chan struct{})}
}

// Acquire returns a permit once fewer reads are in flight than the limit, or ctx.Err().
func (l *Limiter) Acquire(ctx context.Context) (Permit, error) {
	for {
		l.mu.Lock()
		if l.inFlight < l.limit {
			l.inFlight++
			permit := Permit{limiter: l, decreases: l.decreases}
			l.mu.Unlock()
			return permit, nil
		}
		freed := l.freed
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			return Permit{}, ctx.Err()
		case <-freed:
		}
	}
}

// Release ends the permit's read, adjusts the limit, and returns it before and after.
func (p Permit) Release(overloaded bool) (before, after int) {
	l := p.limiter
	l.mu.Lock()
	defer l.mu.Unlock()

	before = l.limit
	l.inFlight--
	switch {
	case !overloaded:
		l.limit = min(l.limit+1, l.ceiling)
	case p.decreases == l.decreases:
		l.limit = max(l.limit/2, l.floor)
		l.decreases++
	}
	close(l.freed)
	l.freed = make(chan struct{})
	return before, l.limit
}

// Limit returns the current limit.
func (l *Limiter) Limit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

// InFlight returns how many permits are held.
func (l *Limiter) InFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inFlight
}
