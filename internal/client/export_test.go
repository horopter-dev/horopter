package client

import (
	"testing"
	"time"
)

// SetHTTPTimeout shortens httpTimeout for one test.
func SetHTTPTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	saved := httpTimeout
	httpTimeout = d
	t.Cleanup(func() { httpTimeout = saved })
}

// SetAttemptTimeout shortens attemptTimeout for one test.
func SetAttemptTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	saved := attemptTimeout
	attemptTimeout = d
	t.Cleanup(func() { attemptTimeout = saved })
}
