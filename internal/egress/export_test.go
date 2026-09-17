package egress

import (
	"context"
	"time"
)

// SetAfterAdmissionHook installs fn as the hook admit calls once a read holds its concurrency
// permit, or clears it when fn is nil. Holding a read there is how a test fills the limit.
func SetAfterAdmissionHook(fn func()) { afterAdmissionHook = fn }

// RetryAfterSeconds exposes retryAfterSeconds.
var RetryAfterSeconds = retryAfterSeconds

// ReadConcurrencyLimit returns s's current read concurrency limit.
func ReadConcurrencyLimit(s *Server) int { return s.limiter.Limit() }

// ReadOverloaded reports whether a read whose connection acquisitions waited waits, all
// succeeding, counts as overloaded against threshold.
func ReadOverloaded(threshold time.Duration, waits ...time.Duration) bool {
	state := &readState{}
	observe := state.connectionWaitObserver(context.Background())
	for _, wait := range waits {
		observe(wait, nil)
	}
	return state.overloaded(threshold)
}
