package ingress

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2/event"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
)

// postCloudEvent ingests a CloudEvents-structured JSON body.
//
// CloudEvents is an envelope, not a complete model: it carries an instant but no
// duration or lifecycle. correlation_ref travels in data, which is what lets two
// CloudEvents describe one interval.
func (s *Server) postCloudEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeBodyError(w, err, "read request body: "+err.Error())
		return
	}
	var ce cloudevents.Event
	if err := json.Unmarshal(raw, &ce); err != nil {
		writeError(w, http.StatusBadRequest, "malformed CloudEvent: "+err.Error()+
			"; expected application/cloudevents+json")
		return
	}
	// source and id together become the idempotency key below, and an envelope missing
	// either still produces a NON-EMPTY key — idempotencyKey("src", "") is "3:src/", not
	// "" — so findExistingDuplicate dedupes on it rather than skipping dedup as it does
	// for an empty key. core.Event.Validate does not check the key either. So without
	// this validation every id-less envelope from one source collapses into whichever
	// arrived first, and each later one is answered
	// 200 {"created": false}: a distinct fact silently dropped from a system of record,
	// which is precisely the failure the composite key exists to eliminate. ce.Validate()
	// enforces the spec's REQUIRED fields (id, source, type), so the degenerate key can
	// never be minted in the first place.
	if err := ce.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid CloudEvent: "+
			strings.TrimSpace(err.Error()))
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	now := s.clock()
	ev, err := eventFromCloudEvent(
		ce, now, core.NewULID(now, s.entropy), p, s.allowLocusPrefixedRef)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !subjectAllowed(p.SubjectPattern, ev.Subject) {
		subjectForbidden(w, p.SubjectPattern, ev.Subject)
		return
	}
	recordTenant(r.Context(), ev.Tenant)
	s.append(w, r, ev)
}

// idempotencyKey encodes a CloudEvent's (source, id) pair into the single TEXT value the
// store dedupes on, injectively: distinct pairs can never produce the same key.
//
// Both halves, not the id alone: CloudEvents guarantees uniqueness of source + id, so ids
// like "1" or a bare build number are entirely conformant. Keying on the id by itself
// silently swallowed a second producer's genuinely distinct event and answered it
// 200 {"created": false} — a dropped fact in a system of record, reported as a successful
// retry.
//
// Joining them under a separator alone does not close that hole, it only narrows it.
// CloudEvents source is a URI-reference, so embedded slashes are the ordinary case rather
// than an exotic one, and source "github/repo" with id "123" would collide with source
// "github" and id "repo/123" under a plain "/" join — the same swallowed event, reached a
// different way. Writing source's byte length first pins where source ends whatever
// either half contains, which is what makes the encoding injective. The "/" that follows
// is for legibility in a log or a table only; it carries no meaning.
//
// A NUL separator would be injective too and is deliberately not used: PostgreSQL TEXT
// cannot store 0x00, so such a key would fail at the INSERT rather than dedupe anything.
//
// The key SHARES one namespace with POST /v1/events, whose idempotency_key is whatever
// the caller sent, verbatim. Both land in the same event.idempotency_key column under the
// same uq_event_idem index, so a native caller who sends exactly "24:/buildkite/pipelines/
// api/build-7" collides with the CloudEvent that derives that string, and one of the two
// events is answered 200 {"created": false} without being stored. This is known and
// accepted for v0.1 rather than fixed: closing it means prefixing the namespaces, and the
// stored key is caller-visible — eventsToJSON emits idempotency_key — so a native caller
// sending "abc" would read back "n:abc". A visible API wart, or strip-on-read complexity,
// in exchange for a collision that a caller has to construct our internal encoding by
// hand to reach.
func idempotencyKey(source, id string) string {
	return strconv.Itoa(len(source)) + ":" + source + "/" + id
}

// eventFromCloudEvent maps a CloudEvent onto the internal Event. Its source and id become
// the idempotency key, which makes producer retries safe for free. Tenant and origin come
// from p, the resolved credential, never from the envelope: see postCloudEvent.
func eventFromCloudEvent(
	ce cloudevents.Event, now time.Time, id string, p auth.Principal, allowLocusPrefix bool,
) (core.Event, error) {
	attrs := map[string]any{}
	if len(ce.Data()) > 0 {
		if err := json.Unmarshal(ce.Data(), &attrs); err != nil {
			// Wrapped with core.ErrInvalidEvent because it is the same class of fact as
			// a missing required field: the caller's own body is wrong, and no retry of
			// it will ever succeed. The wrap changes no status code today — the sole
			// caller, postCloudEvent above, answers every error from here 400
			// unconditionally — so what it buys is that the taxonomy stays true if a
			// future caller routes this error through appendFailed, which reports an
			// unwrapped error as a retryable 503.
			return core.Event{}, fmt.Errorf(
				"%w: CloudEvent %q: data is not a JSON object: %w",
				core.ErrInvalidEvent, ce.ID(), err)
		}
	}
	// A body still carrying tenant is rejected outright, whatever its type: silently
	// dropping a field that used to be authoritative is how a broken migration ships
	// unnoticed, with the caller believing it wrote one tenant while it wrote another.
	if _, present := attrs["tenant"]; present {
		return core.Event{}, fmt.Errorf(
			"%w: CloudEvent %q: tenant is derived from the credential; remove it from data",
			core.ErrInvalidEvent, ce.ID())
	}
	// pull lifts a recognised key out of the data payload into a first-class event
	// field, removing it from the attribute map so the same value is not also stored as
	// a generic attribute. The lifted set is exactly the core.Event fields CloudEvents
	// has no envelope attribute for: correlation_ref, title, actor and lifecycle. Every
	// other key stays in attributes untouched.
	//
	// Non-string values are left alone rather than coerced: a caller sending
	// {"title": 7} keeps it as an attribute instead of having it silently stringified
	// into a field the schema promises is a title.
	pull := func(key string) string {
		if v, ok := attrs[key].(string); ok {
			delete(attrs, key)
			return v
		}
		return ""
	}
	corr, err := resolveRef(
		core.LocusFromRow(p.Locus), pull("correlation_ref"), allowLocusPrefix)
	if err != nil {
		// Wrapped for the same reason as the data-not-JSON-object case above: the taxonomy
		// must hold even though the sole caller today answers every error from here 400
		// unconditionally, or an over-cap ref reaching appendFailed by some future path
		// would be reported as a retryable 503 that can never succeed.
		return core.Event{}, fmt.Errorf("%w: CloudEvent %q: %w", core.ErrInvalidEvent, ce.ID(), err)
	}
	title := pull("title")
	actor := pull("actor")
	lifecycle := pull("lifecycle")

	occurred := ce.Time()
	if occurred.IsZero() {
		occurred = now
	}
	return core.Event{
		ID:             id,
		Tenant:         p.Tenant.String(),
		IdempotencyKey: idempotencyKey(ce.Source(), ce.ID()),
		SchemaVersion:  core.CurrentSchemaVersion,
		Source:         p.Source,
		Type:           ce.Type(),
		Subject:        ce.Subject(),
		Actor:          actor,
		Title:          title,
		OccurredAt:     occurred,
		ReceivedAt:     now,
		CorrelationRef: corr,
		Attributes:     attrs,
		Origin:         p.Origin,
		Locus:          core.LocusFromRow(p.Locus),
		Lifecycle:      core.Lifecycle(lifecycle),
	}, nil
}
