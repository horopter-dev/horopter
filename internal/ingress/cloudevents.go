package ingress

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2/event"
	"github.com/yo61/horopter/internal/core"
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
	// source and id together become the idempotency key below, so an unvalidated
	// envelope missing either would silently get a half-empty key — core.Event.Validate
	// does not check it, and AppendEvent only dedupes a non-empty key — meaning every
	// retry of that envelope creates a new event instead of being recognised as a
	// duplicate. ce.Validate() enforces the spec's REQUIRED fields (id, source, type),
	// catching that before it can happen rather than after it silently has.
	if err := ce.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid CloudEvent: "+
			strings.TrimSpace(err.Error()))
		return
	}
	now := s.clock()
	ev, err := eventFromCloudEvent(ce, now, core.NewULID(now, s.entropy))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.append(w, r, ev)
}

// eventFromCloudEvent maps a CloudEvent onto the internal Event. The CloudEvents source
// and id together become the idempotency key, which makes producer retries safe for free.
//
// Both, not the id alone: CloudEvents guarantees uniqueness of source + id, so ids like
// "1" or a bare build number are entirely conformant. Keying on the id alone meant two
// producers that happened to share one silently swallowed the second, genuinely distinct
// event and answered it 200 {"created": false} — a dropped fact in a system of record,
// reported as a successful retry.
func eventFromCloudEvent(
	ce cloudevents.Event, now time.Time, id string,
) (core.Event, error) {
	attrs := map[string]any{}
	if len(ce.Data()) > 0 {
		if err := json.Unmarshal(ce.Data(), &attrs); err != nil {
			// Wrapped with core.ErrInvalidEvent because it is the same class of fact as
			// a missing required field: the caller's own body is wrong, and no retry of
			// it will ever succeed. Without the wrap it would fall through to 503 and a
			// producer would retry a body that can never be accepted.
			return core.Event{}, fmt.Errorf(
				"%w: CloudEvent %q: data is not a JSON object: %w",
				core.ErrInvalidEvent, ce.ID(), err)
		}
	}
	pull := func(key string) string {
		if v, ok := attrs[key].(string); ok {
			delete(attrs, key)
			return v
		}
		return ""
	}
	tenantID := pull("tenant")
	if tenantID == "" {
		tenantID = DefaultTenant
	}
	corr := pull("correlation_ref")
	title := pull("title")

	occurred := ce.Time()
	if occurred.IsZero() {
		occurred = now
	}
	return core.Event{
		ID:             id,
		Tenant:         tenantID,
		IdempotencyKey: ce.Source() + "/" + ce.ID(),
		SchemaVersion:  core.CurrentSchemaVersion,
		Source:         ce.Source(),
		Type:           ce.Type(),
		Subject:        ce.Subject(),
		Title:          title,
		OccurredAt:     occurred,
		ReceivedAt:     now,
		CorrelationRef: corr,
		Attributes:     attrs,
	}, nil
}
