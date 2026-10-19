package admission

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

// ErrExceedsCapacity reports a reservation larger than the whole pool.
var ErrExceedsCapacity = errors.New("reservation exceeds the pool's capacity")

// BytePool is a fixed number of bytes reads reserve from, serving waiters in arrival order.
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

// Reserve takes n bytes, waiting until free or ctx ends; more than the capacity fails at once.
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
