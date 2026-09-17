package egress

// SetAfterAdmissionHook installs fn as the hook admit calls once a read holds its concurrency
// permit, or clears it when fn is nil. Holding a read there is how a test fills the limit.
func SetAfterAdmissionHook(fn func()) { afterAdmissionHook = fn }

// RetryAfterSeconds exposes retryAfterSeconds.
var RetryAfterSeconds = retryAfterSeconds
