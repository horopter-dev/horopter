package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/admission"
)

func TestBytePoolReservesWithinItsCapacity(t *testing.T) {
	p := admission.NewBytePool(100)

	require.NoError(t, p.Reserve(context.Background(), 60))
	require.NoError(t, p.Reserve(context.Background(), 40))

	assert.Equal(t, int64(100), p.Reserved())
	assert.Equal(t, int64(100), p.Capacity())
}

func TestBytePoolReservationGivesUpAtItsDeadline(t *testing.T) {
	p := admission.NewBytePool(100)
	require.NoError(t, p.Reserve(context.Background(), 60))

	err := p.Reserve(within(t, 20*time.Millisecond), 50)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, int64(60), p.Reserved(), "a reservation that gave up must hold nothing")
}

// TestBytePoolReleaseLetsAWaiterThrough sleeps for the reason
// TestLimiterAdmitsAWaiterWhenAPermitIsReleased does.
func TestBytePoolReleaseLetsAWaiterThrough(t *testing.T) {
	p := admission.NewBytePool(100)
	require.NoError(t, p.Reserve(context.Background(), 60))
	ctx := within(t, 5*time.Second)
	reserved := make(chan error, 1)
	go func() { reserved <- p.Reserve(ctx, 50) }()
	time.Sleep(50 * time.Millisecond)

	p.Release(60)

	select {
	case err := <-reserved:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("releasing bytes must let a waiting reservation through")
	}
	assert.Equal(t, int64(50), p.Reserved())
}

// TestBytePoolRefusesMoreThanItsCapacityWithoutWaiting gives the reservation a context that
// never ends: semaphore.Weighted would wait on it forever.
func TestBytePoolRefusesMoreThanItsCapacityWithoutWaiting(t *testing.T) {
	p := admission.NewBytePool(100)
	refused := make(chan error, 1)
	go func() { refused <- p.Reserve(context.Background(), 101) }()

	select {
	case err := <-refused:
		require.ErrorIs(t, err, admission.ErrExceedsCapacity)
	case <-time.After(time.Second):
		t.Fatal("a reservation larger than the pool must fail at once; it waited instead")
	}
}
