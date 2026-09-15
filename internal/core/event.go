// Package core holds the vendor-neutral domain types. Nothing in this package
// may reference a vendor concept, timestamp unit, or wire format.
package core

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/oklog/ulid/v2"
)

// CurrentSchemaVersion is the only event schema version this build understands.
const CurrentSchemaVersion = 1

// ErrInvalidEvent marks an event the log will never accept however often it is offered:
// a missing required field, an unsupported schema version, a body that is not an object.
// Every such error wraps it with %w so callers can separate "this request is wrong" from
// "this store is unavailable".
//
// That distinction is load-bearing. Producers treat 4xx as "do not retry", so answering a
// transient store failure with the same status as a validation failure makes a database
// blip lose every event posted during it, permanently and with nothing in the log showing
// a hole exists. Only what wraps this sentinel may be reported as the caller's fault.
//
// Its message is written to read as the opening of the wrapping message, which is why the
// wrappers below spell it "%w %q:" rather than appending it.
var ErrInvalidEvent = errors.New("invalid event")

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

// Validate reports whether the event may be appended to the log. Every failure wraps
// ErrInvalidEvent, which is what lets a caller answer it as the caller's own fault rather
// than as a transient store failure.
func (e Event) Validate() error {
	if e.Tenant == "" {
		return fmt.Errorf(
			"%w %q: tenant is required; set it to \"default\" for "+
				"single-tenant deployments", ErrInvalidEvent, e.ID)
	}
	if e.Source == "" {
		return fmt.Errorf("%w %q: source is required, e.g. \"buildkite/pipelines/api\"",
			ErrInvalidEvent, e.ID)
	}
	if e.Type == "" {
		return fmt.Errorf("%w %q: type is required, e.g. \"deployment.started\"",
			ErrInvalidEvent, e.ID)
	}
	if e.Subject == "" {
		return fmt.Errorf(
			"%w %q: subject is required — the entity the change "+
				"happened to, e.g. \"acme/prod\"", ErrInvalidEvent, e.ID)
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("%w %q: occurred_at is required and must be an RFC3339 timestamp",
			ErrInvalidEvent, e.ID)
	}
	if e.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf(
			"%w %q: schema_version %d is not supported; this build "+
				"understands version %d",
			ErrInvalidEvent, e.ID, e.SchemaVersion, CurrentSchemaVersion)
	}
	return nil
}

// NewULID returns a time-sortable 26-character identifier. Callers supply the
// clock and entropy so that generation is testable and deterministic.
func NewULID(t time.Time, entropy io.Reader) string {
	return ulid.MustNew(ulid.Timestamp(t), entropy).String()
}
