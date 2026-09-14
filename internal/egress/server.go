// Package egress is the read-side daemon: it serves queries and renders them through
// vendor adapters.
//
// It contains no write code by design, so a compromise or a bug here cannot corrupt
// the event log.
package egress

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/yo61/horopter/internal/adapter/grafana"
	"github.com/yo61/horopter/internal/adapter/otlp"
	"github.com/yo61/horopter/internal/core"
	"github.com/yo61/horopter/internal/store/postgres"
)

// DefaultTenant is used when a request omits one.
const DefaultTenant = "default"

// Server is the egress HTTP surface.
type Server struct {
	store   *postgres.Store
	clock   func() time.Time
	grafana *grafana.Renderer
	otlp    *otlp.Renderer
}

// New returns an egress Server. clock is injected so rendering an open change stays
// deterministic under test.
func New(s *postgres.Store, clock func() time.Time) *Server {
	return &Server{store: s, clock: clock, grafana: grafana.New(), otlp: otlp.New()}
}

// Routes returns the handler for every egress endpoint.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/changes", s.queryChanges)
	mux.HandleFunc("GET /v1/events", s.listEvents)
	mux.HandleFunc("GET /adapters/grafana/annotations", s.grafanaAnnotations)
	mux.HandleFunc("GET /adapters/otlp/spans", s.otlpSpans)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.readyz)
	return mux
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Pool().Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unreachable: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func tenantOf(r *http.Request) string {
	if v := r.URL.Query().Get("tenant"); v != "" {
		return v
	}
	return DefaultTenant
}

// rfc3339Param parses an RFC3339 query parameter, naming the expected format on error.
func rfc3339Param(r *http.Request, name string) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, fmt.Errorf(
			"query parameter %q is required, as RFC3339, e.g. 2026-09-14T10:00:00Z", name)
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"query parameter %q=%q is not RFC3339: %w", name, raw, err)
	}
	return t, nil
}

// epochMillisParam parses a Grafana-style epoch-millisecond parameter.
func epochMillisParam(r *http.Request, name string) (time.Time, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, fmt.Errorf(
			"query parameter %q is required; Grafana sends it as epoch milliseconds", name)
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"query parameter %q=%q is not epoch milliseconds: %w", name, raw, err)
	}
	return time.UnixMilli(ms).UTC(), nil
}

func (s *Server) changesFor(
	r *http.Request, from, to time.Time,
) ([]core.Change, error) {
	return s.store.QueryChanges(r.Context(), postgres.ChangeQuery{
		Tenant:  tenantOf(r),
		From:    from,
		To:      to,
		Subject: r.URL.Query().Get("subject"),
		Type:    r.URL.Query().Get("type"),
		Status:  r.URL.Query().Get("status"),
	})
}

func (s *Server) queryChanges(w http.ResponseWriter, r *http.Request) {
	from, err := rfc3339Param(r, "from")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	to, err := rfc3339Param(r, "to")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	changes, err := s.changesFor(r, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, changesToJSON(changes))
}

// listEvents returns the raw event log for a tenant, optionally scoped to one
// correlation ref, for audit and export. It reads only — no write path exists here.
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.ListEvents(r.Context(), postgres.EventQuery{
		Tenant: tenantOf(r),
		Ref:    r.URL.Query().Get("ref"),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, eventsToJSON(events))
}

func (s *Server) grafanaAnnotations(w http.ResponseWriter, r *http.Request) {
	from, err := epochMillisParam(r, "from")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	to, err := epochMillisParam(r, "to")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.render(w, r, from, to, s.grafana.Render)
}

func (s *Server) otlpSpans(w http.ResponseWriter, r *http.Request) {
	from, err := rfc3339Param(r, "from")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	to, err := rfc3339Param(r, "to")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.render(w, r, from, to, s.otlp.Render)
}

func (s *Server) render(
	w http.ResponseWriter, r *http.Request, from, to time.Time,
	render func([]core.Change, time.Time) (any, error),
) {
	changes, err := s.changesFor(r, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	payload, err := render(changes, s.clock())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// changesToJSON renders changes in neutral terms. No vendor field names, no epoch
// units — those belong to the adapters.
func changesToJSON(changes []core.Change) []map[string]any {
	out := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		m := map[string]any{
			"ref": c.Ref, "subject": c.Subject, "type": c.Type, "title": c.Title,
			"actor": c.Actor, "start": c.Start.UTC().Format(time.RFC3339Nano),
			"status": string(c.Status), "attributes": c.Attributes,
			"last_event_id": c.LastEventID,
		}
		if c.End != nil {
			m["end"] = c.End.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, m)
	}
	return out
}

// eventsToJSON renders the raw event log in neutral terms, for audit and export.
func eventsToJSON(events []core.Event) []map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{
			"id": e.ID, "tenant": e.Tenant, "idempotency_key": e.IdempotencyKey,
			"schema_version": e.SchemaVersion, "source": e.Source, "type": e.Type,
			"subject": e.Subject, "actor": e.Actor, "title": e.Title,
			"occurred_at":     e.OccurredAt.UTC().Format(time.RFC3339Nano),
			"received_at":     e.ReceivedAt.UTC().Format(time.RFC3339Nano),
			"correlation_ref": e.CorrelationRef, "attributes": e.Attributes,
		})
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
