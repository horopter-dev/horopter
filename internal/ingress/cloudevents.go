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

// CloudEvents carries an instant but no duration: correlation_ref travels in data.
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
	// Validate first: a missing id still mints a non-empty key, deduping distinct events.
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

// Length-prefixed, so distinct (source, id) pairs never collide; not NUL, which TEXT rejects.
func idempotencyKey(source, id string) string {
	return strconv.Itoa(len(source)) + ":" + source + "/" + id
}

// Tenant, origin and locus come from p, never from the envelope.
func eventFromCloudEvent(
	ce cloudevents.Event, now time.Time, id string, p auth.Principal, allowLocusPrefix bool,
) (core.Event, error) {
	attrs := map[string]any{}
	if len(ce.Data()) > 0 {
		if err := json.Unmarshal(ce.Data(), &attrs); err != nil {
			// Wrapped so appendFailed would answer 400, not a retryable 503.
			return core.Event{}, fmt.Errorf(
				"%w: CloudEvent %q: data is not a JSON object: %w",
				core.ErrInvalidEvent, ce.ID(), err)
		}
	}
	// Refused, not ignored: silently dropping a once-authoritative field hides a broken migration.
	if _, present := attrs["tenant"]; present {
		return core.Event{}, fmt.Errorf(
			"%w: CloudEvent %q: tenant is derived from the credential; remove it from data",
			core.ErrInvalidEvent, ce.ID())
	}
	// Lifts the fields CloudEvents has no envelope attribute for; non-string values stay put.
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
		// Wrapped for the same reason as above.
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
