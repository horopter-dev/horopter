package egress

import (
	"context"
	"time"
)

// SetAfterAdmissionHook installs fn as the hook admit calls once a read holds its concurrency
// permit, or clears it when fn is nil. Holding a read there is how a test fills the limit.
func SetAfterAdmissionHook(fn func()) { afterAdmissionHook = fn }

// ReadExpansionFactor exposes readExpansionFactor.
const ReadExpansionFactor = readExpansionFactor

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

// ReadOverloadedByAcquisition reports whether a single connection acquisition that waited wait
// and ended with err, observed under requestCtx, counts as overloaded against threshold.
func ReadOverloadedByAcquisition(
	requestCtx context.Context, threshold, wait time.Duration, err error,
) bool {
	state := &readState{}
	state.connectionWaitObserver(requestCtx)(wait, err)
	return state.overloaded(threshold)
}
