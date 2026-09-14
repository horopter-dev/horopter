// Package core holds the vendor-neutral domain types. Nothing in this package
// may reference a vendor concept, timestamp unit, or wire format.
package core

import (
	"fmt"
	"io"
	"time"

	"github.com/oklog/ulid/v2"
)

// CurrentSchemaVersion is the only event schema version this build understands.
const CurrentSchemaVersion = 1

// Event is an immutable fact. Events are never updated or deleted.
type Event struct {
	ID             string
	Tenant         string
	IdempotencyKey string
	SchemaVersion  int
	Source         string
	Type           string
	Subject        string
	Actor          string
	Title          string
	OccurredAt     time.Time
	ReceivedAt     time.Time
	CorrelationRef string
	Attributes     map[string]any
}

// Validate reports whether the event may be appended to the log.
func (e Event) Validate() error {
	if e.Tenant == "" {
		return fmt.Errorf(
			"event %q: tenant is required; set it to \"default\" for "+
				"single-tenant deployments", e.ID)
	}
	if e.Source == "" {
		return fmt.Errorf("event %q: source is required, e.g. \"buildkite/pipelines/api\"", e.ID)
	}
	if e.Type == "" {
		return fmt.Errorf("event %q: type is required, e.g. \"deployment.started\"", e.ID)
	}
	if e.Subject == "" {
		return fmt.Errorf(
			"event %q: subject is required — the entity the change "+
				"happened to, e.g. \"acme/prod\"", e.ID)
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("event %q: occurred_at is required and must be an RFC3339 timestamp", e.ID)
	}
	if e.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf(
			"event %q: schema_version %d is not supported; this build "+
				"understands version %d",
			e.ID, e.SchemaVersion, CurrentSchemaVersion)
	}
	return nil
}

// NewULID returns a time-sortable 26-character identifier. Callers supply the
// clock and entropy so that generation is testable and deterministic.
func NewULID(t time.Time, entropy io.Reader) string {
	return ulid.MustNew(ulid.Timestamp(t), entropy).String()
}
