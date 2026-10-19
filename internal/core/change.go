package core

import "time"

// Status is the lifecycle state of a change.
type Status string

// Status values. A point is marked by no correlation ref, never by StatusOccurred or End == Start.
const (
	StatusOccurred   Status = "occurred"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusRolledBack Status = "rolled_back"
	StatusCancelled  Status = "cancelled"
)

// Statuses is the whole vocabulary; a test holds migration 0002's CHECK to this list.
var Statuses = []Status{
	StatusOccurred,
	StatusInProgress,
	StatusCompleted,
	StatusFailed,
	StatusRolledBack,
	StatusCancelled,
}

// OpenSentinel ends an open change so an index can serve overlap; Go cannot hold infinity.
var OpenSentinel = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// Change is the rebuildable projection every render adapter reads: a point, open or closed.
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
	// Sources is every system that reported the change, as a sorted set so order cannot matter.
	Sources []string
	// SubjectFirstSeen maps each subject to its earliest occurred_at, in Unix microseconds.
	SubjectFirstSeen map[string]int64
	// TitleStartedAt is when the start that titled the change occurred, nil for a fallback title.
	TitleStartedAt *time.Time
}

// IsOpen reports whether the change is still in flight.
func (c Change) IsOpen() bool { return c.End == nil }
