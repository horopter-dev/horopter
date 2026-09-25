// Package core holds the vendor-neutral domain types. Nothing in this package
// may reference a vendor concept, timestamp unit, or wire format.
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

// Lifecycle is what an event asserts about its change's lifecycle, stated by the producer
// rather than inferred from the type's last segment.
//
// The values are TRANSITIONS, not statuses. There is deliberately no "occurred" or
// "in_progress": an event cannot assert that a change IS in progress, only that it
// started. The status is what Fold derives.
type Lifecycle string

// LifecycleUnspecified is absence: the producer said nothing and the type suffix
// decides. LifecycleNone is an assertion that nothing happened, which overrides the
// suffix. They must stay distinct — collapsing them makes "explicit beats convention"
// unimplementable, because an omitted JSON key and an explicit "" decode alike.
const (
	LifecycleUnspecified Lifecycle = ""
	LifecycleNone        Lifecycle = "none"
	LifecycleStarted     Lifecycle = "started"
	LifecycleCompleted   Lifecycle = "completed"
	LifecycleFailed      Lifecycle = "failed"
	LifecycleRolledBack  Lifecycle = "rolled_back"
	LifecycleCancelled   Lifecycle = "cancelled"
)

// Lifecycles is every value a producer may assert, in the order migration 0008's CHECK
// lists them. An eighth added here without a migration compiles, passes, and fails at
// runtime on the first insert — which is what TestChangeStatusVocabularyIsEnforced was
// written for after exactly that happened to Status.
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
	// Origin is derived from the appending credential's type, never from the request
	// body: a caller-asserted origin would be worthless, since a producer can post any
	// attribute it likes.
	Origin Origin
	// Locus is derived from the appending credential, never from the request body: a
	// caller-asserted locus would defeat the namespacing it exists to provide, which is
	// the argument Origin makes for itself. RefFor composes a point change's ref from it,
	// so it is part of what the event is, not a denormalised copy of a credential field.
	Locus Locus
	// Lifecycle is what this event asserts about its change. Empty means the producer
	// said nothing, in which case the type's suffix decides — which is every event
	// stored before migration 0008.
	Lifecycle Lifecycle
	// Edited is false on every append. v0.2's edit path is what sets it true; nothing
	// in this build does.
	Edited bool
}

// Validate reports whether the event may be appended to the log. Every failure wraps
// ErrInvalidEvent, which is what lets a caller answer it as the caller's own fault rather
// than as a transient store failure.
//
// The id check only catches a malformed id, never a colliding one: ParseStrict rejects
// anything that is not a syntactically valid 26-character ULID, but two distinct events
// can still present the same well-formed id, and this is not where that would be caught.
// Every write reaches the store through AppendEvent, which calls Validate first, so this
// is the only enforcement point the id format has — there is no database CHECK
// constraint for it. That leaves a second writer bypassing this package as the residual
// risk; none exists today, and adding a migration to guard against one has a real cost
// (a schema version bump, daemon coordination) for a hazard that is currently
// hypothetical.
//
// Validate does not guard CorrelationRef against event_corr_ref_has_locus (a non-empty
// correlation_ref must carry a slash after its first character): Ref's COMPOSING
// constructors are that guard now, not the type. NewRef and ParseRef both refuse a value
// without a locus, and the only other source, RefFromRow, carries a value the constraint
// already passed. There is nothing left for Validate to add.
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
	// Checked here so it is the caller's fault, not the store's. event_origin_vocabulary
	// rejects an empty origin, and a constraint violation reaches ingress as a store failure
	// — a 503 telling the producer to retry a request that can never succeed.
	if err := e.Origin.Validate(); err != nil {
		return fmt.Errorf("%w %q: %w; origin is derived from the appending credential",
			ErrInvalidEvent, e.ID, err)
	}
	// Checked here for the same reason as Origin: event_locus_shape rejects an invalid
	// locus, and a constraint violation reaches ingress as a store failure — a 503 telling
	// the producer to retry a request that can never succeed.
	if err := e.validateLocus(); err != nil {
		return err
	}
	return nil
}

// validateLocus reports whether the event carries a locus at all. That is the one thing no
// constructor can settle; the shape rules the ref encoding requires — non-empty, no "/" —
// are settled before a Locus exists, so re-checking them here would restate a rule rather
// than enforce one. It wraps the failure so the error names the event, as every other
// Validate failure does.
//
// Be precise about WHERE those rules are held, because it is not the type. NewLocus holds
// them, and so does ParseRef through it; LocusFromRow does NOT, by design and for the same
// reason TenantIDFromRow does not — failing a read on a stored value would make a row
// unreadable rather than unwritable. LocusFromRow is exported, so core.LocusFromRow("a/b")
// mints a slash-bearing Locus from outside this package today.
//
// What keeps that out of a production event is a CROSS-FILE INVARIANT, not the compiler.
// Only two sites set Locus on an event that reaches this check — internal/ingress's
// server.go and cloudevents.go — and both set it from the credential alone. (The store's
// scans set it too, but from rows event_locus_shape has already passed, and a read does
// not call Validate.) A credential's locus is in turn held slash-free twice over: by
// auth.ValidateLocus when horoctl issues one, and by credential_locus_shape (migration
// 0004) when the row is written. Anything that adds a third writer of Locus — or that
// relaxes either of those two — owes this check the rule back.
//
// The fuller rule an operator is held to — the reserved "legacy", a length cap, edge
// whitespace — is auth.ValidateLocus's too, and is checked when a credential is issued
// rather than when an event is appended.
func (e Event) validateLocus() error {
	if e.Locus.IsZero() {
		return fmt.Errorf("%w %q: locus is required; it is derived from the appending "+
			"credential", ErrInvalidEvent, e.ID)
	}
	return nil
}

// validateFieldShapes reports the first required text field the event leaves empty, naming
// an example of what it should carry, or an out-of-vocabulary lifecycle if the text fields
// are fine. Split out of Validate to keep its branching within the complexity limit.
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

// CheckAttributeSize reports whether the event's attributes fit within limit bytes of JSON.
// Every failure wraps ErrInvalidEvent: an event over the limit stays over it however often it
// is offered, so the producer must not retry it.
//
// The size is compact JSON without HTML escaping, which is the form a producer sends, so the
// number in the error is one the producer can check against its own payload. encoding/json
// escapes <, > and & as six bytes each by default, which would count bytes nobody sent. A nil
// map measures as {}, the value the store writes for it.
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

// NewULID returns a time-sortable 26-character identifier. Callers supply the
// clock and entropy so that generation is testable and deterministic.
func NewULID(t time.Time, entropy io.Reader) string {
	return ulid.MustNew(ulid.Timestamp(t), entropy).String()
}
