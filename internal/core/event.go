// Package core holds the vendor-neutral domain types: no vendor concept, time unit or wire format.
package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/oklog/ulid/v2"
)

// CurrentSchemaVersion is the only event schema version this build understands.
const CurrentSchemaVersion = 2

// ErrInvalidEvent marks an event no retry can fix; only a validation failure may wrap it.
var ErrInvalidEvent = errors.New("invalid event")

// Lifecycle is the transition an event asserts about its change, not a status.
type Lifecycle string

// LifecycleUnspecified (absent) and LifecycleNone (asserts nothing) must stay distinct.
const (
	LifecycleUnspecified Lifecycle = ""
	LifecycleNone        Lifecycle = "none"
	LifecycleStarted     Lifecycle = "started"
	LifecycleCompleted   Lifecycle = "completed"
	LifecycleFailed      Lifecycle = "failed"
	LifecycleRolledBack  Lifecycle = "rolled_back"
	LifecycleCancelled   Lifecycle = "cancelled"
)

// Lifecycles is every value a producer may assert, in migration 0008's CHECK order.
var Lifecycles = []Lifecycle{
	LifecycleUnspecified, LifecycleNone, LifecycleStarted, LifecycleCompleted,
	LifecycleFailed, LifecycleRolledBack, LifecycleCancelled,
}

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
	CorrelationRef Ref
	Attributes     map[string]any
	// Origin comes from the appending credential's type, never from the request body.
	Origin Origin
	// Locus comes from the appending credential, never from the request body.
	Locus Locus
	// Lifecycle is empty on every event stored before migration 0008.
	Lifecycle Lifecycle
	// Edited is set only by v0.2's edit path; nothing in this build sets it.
	Edited bool
}

// Validate reports whether the event may be appended; every failure wraps ErrInvalidEvent.
func (e Event) Validate() error {
	if _, err := ulid.ParseStrict(e.ID); err != nil {
		return fmt.Errorf("%w %q: id must be a 26-character ULID: %w",
			ErrInvalidEvent, e.ID, err)
	}
	if e.Tenant == "" {
		return fmt.Errorf(
			"%w %q: tenant is required; it comes from the appending credential, so this "+
				"credential is not bound to one", ErrInvalidEvent, e.ID)
	}
	if err := e.validateFieldShapes(); err != nil {
		return err
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
	// Checked here, not by event_origin_vocabulary, so the producer gets a 400 rather than a 503.
	if err := e.Origin.Validate(); err != nil {
		return fmt.Errorf("%w %q: %w; origin is derived from the appending credential",
			ErrInvalidEvent, e.ID, err)
	}
	// Likewise for event_locus_shape.
	if err := e.validateLocus(); err != nil {
		return err
	}
	return nil
}

// validateLocus requires a locus; its no-slash rule rests on ingress setting it from credentials.
func (e Event) validateLocus() error {
	if e.Locus.IsZero() {
		return fmt.Errorf("%w %q: locus is required; it is derived from the appending "+
			"credential", ErrInvalidEvent, e.ID)
	}
	return nil
}

// validateFieldShapes reports the first empty required field, or an unknown lifecycle.
func (e Event) validateFieldShapes() error {
	switch {
	case e.Source == "":
		return fmt.Errorf("%w %q: source is required, e.g. \"buildkite/pipelines/api\"",
			ErrInvalidEvent, e.ID)
	case e.Type == "":
		return fmt.Errorf("%w %q: type is required, e.g. \"deployment.started\"",
			ErrInvalidEvent, e.ID)
	case e.Subject == "":
		return fmt.Errorf(
			"%w %q: subject is required — the entity the change "+
				"happened to, e.g. \"acme/prod\"", ErrInvalidEvent, e.ID)
	case !slices.Contains(Lifecycles, e.Lifecycle):
		return fmt.Errorf("%w %q: lifecycle %q is not one of %v",
			ErrInvalidEvent, e.ID, e.Lifecycle, Lifecycles)
	}
	return nil
}

// CheckAttributeSize reports whether the attributes fit in limit bytes of compact, unescaped JSON.
func (e Event) CheckAttributeSize(limit int) error {
	size, err := attributeJSONSize(e.Attributes)
	if err != nil {
		return fmt.Errorf("%w %q: attributes cannot be encoded as JSON: %w",
			ErrInvalidEvent, e.ID, err)
	}
	if size > limit {
		return fmt.Errorf(
			"%w %q: attributes encode to %d bytes of JSON; the limit is %d — keep "+
				"attributes to identifiers and short text, and link to anything larger",
			ErrInvalidEvent, e.ID, size, limit)
	}
	return nil
}

// attributeJSONSize returns the length of attrs as compact, unescaped JSON.
func attributeJSONSize(attrs map[string]any) (int, error) {
	if attrs == nil {
		attrs = map[string]any{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(attrs); err != nil {
		return 0, err
	}
	// Encode ends its output with a newline that is not part of the JSON value.
	return buf.Len() - 1, nil
}

// NewULID returns a ULID from the caller's clock and entropy, so generation is deterministic.
func NewULID(t time.Time, entropy io.Reader) string {
	return ulid.MustNew(ulid.Timestamp(t), entropy).String()
}
