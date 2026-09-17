package admission

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

// ErrExceedsCapacity reports a reservation larger than the whole pool, which no amount of
// waiting could satisfy.
var ErrExceedsCapacity = errors.New("reservation exceeds the pool's capacity")

// BytePool is a fixed number of bytes that reads reserve from while they run.
//
// It is built on semaphore.Weighted, which serves waiters in the order they arrived, so a
// stream of small reservations cannot starve a large one.
type BytePool struct {
	capacity int64
	sem      *semaphore.Weighted
	// reserved mirrors the semaphore's count, which semaphore.Weighted does not expose.
	reserved atomic.Int64
}

// NewBytePool returns a pool of capacity bytes, none of them reserved.
func NewBytePool(capacity int64) *BytePool {
	return &BytePool{capacity: capacity, sem: semaphore.NewWeighted(capacity)}
}

// Reserve takes n bytes, waiting until they are free or ctx ends. A reservation larger than
// the pool fails at once with ErrExceedsCapacity: semaphore.Weighted would otherwise wait for
// ctx to end, so a read that can never fit would look like one refused for being busy.
func (p *BytePool) Reserve(ctx context.Context, n int64) error {
	if n > p.capacity {
		return fmt.Errorf("%w: %d bytes requested of %d", ErrExceedsCapacity, n, p.capacity)
	}
	if err := p.sem.Acquire(ctx, n); err != nil {
		return err
	}
	p.reserved.Add(n)
	return nil
}

// Release returns n bytes reserved earlier.
func (p *BytePool) Release(n int64) {
	p.reserved.Add(-n)
	p.sem.Release(n)
}

// Reserved returns how many bytes are reserved.
func (p *BytePool) Reserved() int64 { return p.reserved.Load() }

// Capacity returns the pool's size in bytes.
func (p *BytePool) Capacity() int64 { return p.capacity }
