package egress

import (
	"context"
	"time"
)

// SetAfterAdmissionHook installs fn as the hook admit calls once a read holds its permit.
func SetAfterAdmissionHook(fn func()) { afterAdmissionHook = fn }

// ReadExpansionFactor exposes readExpansionFactor.
const ReadExpansionFactor = readExpansionFactor

// RetryAfterSeconds exposes retryAfterSeconds.
var RetryAfterSeconds = retryAfterSeconds

// ReadConcurrencyLimit returns s's current read concurrency limit.
func ReadConcurrencyLimit(s *Server) int { return s.limiter.Limit() }

// InFlight returns how many reads hold a concurrency permit.
func InFlight(s *Server) int { return s.limiter.InFlight() }

// ReadOverloaded reports whether successful acquisitions that waited count as overloaded.
func ReadOverloaded(threshold time.Duration, waits ...time.Duration) bool {
	state := &readState{}
	observe := state.connectionWaitObserver(context.Background())
	for _, wait := range waits {
		observe(wait, nil)
	}
	return state.overloaded(threshold)
}

// ReadOverloadedByAcquisition reports whether one acquisition counts as overloaded.
func ReadOverloadedByAcquisition(
	requestCtx context.Context, threshold, wait time.Duration, err error,
) bool {
	state := &readState{}
	state.connectionWaitObserver(requestCtx)(wait, err)
	return state.overloaded(threshold)
}

// ReserveReadMemory reserves n bytes of s's read memory pool and returns the release.
func ReserveReadMemory(s *Server, n int64) (func(), error) {
	if err := s.memory.Reserve(context.Background(), n); err != nil {
		return nil, err
	}
	return func() { s.memory.Release(n) }, nil
}

// ReadMemoryReserved returns how many bytes of s's read memory pool are reserved.
func ReadMemoryReserved(s *Server) int64 { return s.memory.Reserved() }
