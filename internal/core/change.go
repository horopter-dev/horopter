package core

import "time"

// Status is the lifecycle state of a change.
type Status string

// The Status values are the lifecycle a change can pass through. StatusOccurred marks
// an instantaneous change with no correlation ref (a point, never an interval).
// StatusInProgress, StatusCompleted, StatusFailed, StatusRolledBack and StatusCancelled
// are the possible outcomes of a correlated change that has a start and, once one of
// the terminal statuses is folded in, an end.
//
// StatusOccurred is the ONLY way to recognise a point. Timestamp equality is not:
// a same-second start/complete pair — routine in backfills and from idempotent
// producers — is a closed interval of zero duration whose End also equals its Start.
// Only the no-correlation-ref path in Fold ever sets StatusOccurred. The rule matters
// to a renderer that draws points and regions differently; neither adapter in this
// repository needs to, because a zero-duration region already renders the way a point
// should.
const (
	StatusOccurred   Status = "occurred"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusRolledBack Status = "rolled_back"
	StatusCancelled  Status = "cancelled"
)

// Statuses is the complete status vocabulary, in the order the constants above declare
// it. It exists so that anything needing the whole set derives it from here rather than
// copying it: migration 0002's CHECK constraint restates this list in SQL, and the only
// thing keeping the two in agreement is a test that iterates this slice. Adding a Status
// constant without adding it here leaves that test passing against a stale list, which is
// exactly the drift it exists to catch.
var Statuses = []Status{
	StatusOccurred,
	StatusInProgress,
	StatusCompleted,
	StatusFailed,
	StatusRolledBack,
	StatusCancelled,
}

// OpenSentinel stands in for the end of an open change so that the overlap
// predicate can be served by a plain index. It is a far-future literal rather
// than Postgres 'infinity' because Go's time.Time cannot represent infinity.
var OpenSentinel = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// Change is the derived, rebuildable projection that every render adapter
// reads. It takes one of three shapes: point, open, or closed.
//
// A change's start is two facts rather than one. StartedAt is the ASSERTION —
// a start-type event's own occurred_at — and is nil until such an event
// arrives. FirstSeenAt is the INFERENCE: the earliest occurred_at of any event
// folded onto the ref, evidence that the change must have existed by then.
// Start is COALESCE(StartedAt, FirstSeenAt) and mirrors the generated start_at
// column, so consumers that only want "when did this start" read Start and are
// unaffected by the split. FirstSeenAt < StartedAt is a real and queryable
// anomaly — an event claiming to predate the change's declared start — rather
// than something the fold has erased.
//
// The split exists because one field held two different kinds of claim: a
// producer's assertion that the change began at T, and an inference that it
// must have existed by T because evidence of it does. Any rule written over a
// single field has to destroy one of them — earliest-always discards the
// assertion, last-write-wins discards the earlier evidence — and the choice was
// being forced by the data model rather than by the domain. Keeping both facts
// removes the dilemma instead of resolving it.
type Change struct {
	Tenant      string
	Ref         Ref
	Subject     string
	Type        string
	Title       string
	Actor       string
	Start       time.Time
	StartedAt   *time.Time
	FirstSeenAt time.Time
	End         *time.Time
	Status      Status
	Attributes  map[string]any
	LastEventID string
}

// IsOpen reports whether the change is still in flight. An open change renders
// as a region growing to the present.
func (c Change) IsOpen() bool { return c.End == nil }
