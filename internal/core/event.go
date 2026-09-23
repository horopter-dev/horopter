// Package core holds the vendor-neutral domain types. Nothing in this package
// may reference a vendor concept, timestamp unit, or wire format.
package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
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
	// Origin is derived from the appending credential's type, never from the request
	// body: a caller-asserted origin would be worthless, since a producer can post any
	// attribute it likes.
	Origin Origin
	// Locus is derived from the appending credential, never from the request body: a
	// caller-asserted locus would defeat the namespacing it exists to provide, which is
	// the argument Origin makes for itself. RefFor composes a point change's ref from it,
	// so it is part of what the event is, not a denormalised copy of a credential field.
	Locus string
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
// correlation_ref must carry a slash after its first character) the way it guards Locus
// against event_locus_shape below. The same argument applies — a constraint violation
// reaches ingress as a store failure, a 503 telling the producer to retry a request that
// can never succeed — but nothing reaches Validate with a CorrelationRef that could trip
// it: resolveRef (internal/ingress/server.go) is the only production composer, and it
// returns either an empty string or the locus and ref joined with a slash. A future
// caller that appends directly, bypassing resolveRef, would need this guard added rather
// than assuming Validate already provides it.
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
	if err := e.validateRequiredText(); err != nil {
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

// validateLocus reports whether the event's locus is well-formed: non-empty, and free of
// "/", which RefFor uses as the separator when composing a point's ref from it. The full
// rule (the reserved "legacy", the length cap, edge whitespace) lives in
// auth.ValidateLocus, which core cannot call: internal/auth imports internal/core, not the
// other way round. Split out of Validate to keep its branching within the complexity
// limit, same as validateRequiredText below.
func (e Event) validateLocus() error {
	if e.Locus == "" || strings.Contains(e.Locus, "/") {
		return fmt.Errorf(
			"%w %q: locus %q must be non-empty and must not contain /; it is derived "+
				"from the appending credential", ErrInvalidEvent, e.ID, e.Locus)
	}
	return nil
}

// validateRequiredText reports the first required text field the event leaves empty, naming
// an example of what it should carry. Split out of Validate to keep its branching within the
// complexity limit.
func (e Event) validateRequiredText() error {
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
