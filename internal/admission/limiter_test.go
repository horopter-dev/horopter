package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/horopter-dev/horopter/internal/admission"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// within returns a context that ends after d, for asserting that a wait gives up.
func within(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func acquire(t *testing.T, l *admission.Limiter) admission.Permit {
	t.Helper()
	p, err := l.Acquire(context.Background())
	require.NoError(t, err)
	return p
}

func TestLimiterAdmitsUpToTheLimit(t *testing.T) {
	l := admission.NewLimiter(1, 2, 4)
	acquire(t, l)
	acquire(t, l)

	_, err := l.Acquire(within(t, 20*time.Millisecond))

	require.ErrorIs(t, err, context.DeadlineExceeded,
		"a third read must wait, then give up at its deadline")
	assert.Equal(t, 2, l.InFlight())
}

// TestLimiterAdmitsAWaiterWhenAPermitIsReleased sleeps to let the waiter start waiting. Had it
// not started, it would be admitted at once and the test would still pass, so the sleep can
// only weaken what the test proves, never make it fail spuriously.
func TestLimiterAdmitsAWaiterWhenAPermitIsReleased(t *testing.T) {
	l := admission.NewLimiter(1, 1, 1)
	held := acquire(t, l)
	ctx := within(t, 5*time.Second)
	admitted := make(chan error, 1)
	go func() {
		_, err := l.Acquire(ctx)
		admitted <- err
	}()
	time.Sleep(50 * time.Millisecond)

	held.Release(false)

	select {
	case err := <-admitted:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("releasing a permit must wake a waiting Acquire")
	}
}

func TestLimiterRaisesTheLimitByOneOnAHealthyRelease(t *testing.T) {
	l := admission.NewLimiter(1, 2, 3)

	before, after := acquire(t, l).Release(false)
	assert.Equal(t, [2]int{2, 3}, [2]int{before, after})

	before, after = acquire(t, l).Release(false)
	assert.Equal(t, [2]int{3, 3}, [2]int{before, after}, "the limit must stop at its ceiling")
}

func TestLimiterHalvesTheLimitOnAnOverloadedRelease(t *testing.T) {
	l := admission.NewLimiter(2, 8, 16)

	before, after := acquire(t, l).Release(true)
	assert.Equal(t, [2]int{8, 4}, [2]int{before, after})

	before, after = acquire(t, l).Release(true)
	assert.Equal(t, [2]int{4, 2}, [2]int{before, after})

	before, after = acquire(t, l).Release(true)
	assert.Equal(t, [2]int{2, 2}, [2]int{before, after}, "the limit must stop at its floor")
	assert.Equal(t, 2, l.Limit())
}

// TestLimiterHalvesOnceForReadsAdmittedBeforeTheSameDecrease is the rule that keeps one
// congestion episode from collapsing the limit: every read queued behind it reports it.
func TestLimiterHalvesOnceForReadsAdmittedBeforeTheSameDecrease(t *testing.T) {
	l := admission.NewLimiter(1, 8, 16)
	first, second := acquire(t, l), acquire(t, l)

	before, after := first.Release(true)
	assert.Equal(t, [2]int{8, 4}, [2]int{before, after})

	before, after = second.Release(true)
	assert.Equal(t, [2]int{4, 4}, [2]int{before, after},
		"a read admitted before the decrease reported the congestion that caused it")

	before, after = acquire(t, l).Release(true)
	assert.Equal(t, [2]int{4, 2}, [2]int{before, after},
		"a read admitted after the decrease reports new congestion")
}
