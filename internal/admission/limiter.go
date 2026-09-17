// Package admission decides whether egress has the capacity to serve a read now. It holds the
// two gates a read passes — a concurrency limit and a memory pool — and nothing else: no
// telemetry, logging, clock or database, which egress wires around it.
package admission

import (
	"context"
	"sync"
)

// Limiter is an additive-increase, multiplicative-decrease concurrency limit. A read that
// finishes healthy raises the limit by one, up to a ceiling; a read that finishes overloaded
// halves it, down to a floor.
//
// Every read that queued behind one congestion episode reports it when it finishes, so halving
// on each report would collapse the limit to its floor from a single episode. A permit therefore
// records how many decreases had happened when it was issued, and an overloaded report from a
// permit issued before the latest decrease changes nothing.
type Limiter struct {
	mu        sync.Mutex
	floor     int
	ceiling   int
	limit     int
	inFlight  int
	decreases uint64
	// freed is closed, and replaced, whenever a permit may have become free, waking every
	// waiting Acquire to check again.
	freed chan struct{}
}

// Permit is one admitted read's hold on a Limiter. Release it exactly once.
type Permit struct {
	limiter   *Limiter
	decreases uint64
}

// NewLimiter returns a Limiter whose limit starts at initial. It requires
// 1 <= floor <= initial <= ceiling.
func NewLimiter(floor, initial, ceiling int) *Limiter {
	return &Limiter{floor: floor, ceiling: ceiling, limit: initial, freed: make(chan struct{})}
}

// Acquire returns a permit once fewer reads are in flight than the limit allows, or ctx.Err()
// if ctx ends first.
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

// Release ends the permit's read and adjusts the limit by whether the read was overloaded. It
// returns the limit before and after, so the caller can report a decrease.
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
