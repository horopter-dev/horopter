// Package ingress is the write-side daemon: it receives events, validates them, and
// appends them to the log, folding each into the projection in the same transaction.
//
// It contains no query code by design. Egress cannot write and ingress cannot read,
// so a compromise or a bug in either is structurally confined to one half.
package ingress

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yo61/horopter/internal/core"
	"github.com/yo61/horopter/internal/store/postgres"
)

// DefaultTenant is used when a request omits one. v0.1 runs single-tenant in practice,
// but every write is still tenant-scoped so enabling multi-tenancy is configuration.
const DefaultTenant = "default"

// Server is the ingress HTTP surface.
type Server struct {
	store   *postgres.Store
	clock   func() time.Time
	entropy io.Reader
	token   string
}

// New returns an ingress Server. clock and entropy are injected so handlers stay
// deterministic under test.
func New(s *postgres.Store, clock func() time.Time, entropy io.Reader, token string) *Server {
	return &Server{store: s, clock: clock, entropy: entropy, token: token}
}

// Routes returns the handler for every ingress endpoint. Probes are unauthenticated;
// everything else requires the bearer token.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/events", s.auth(http.HandlerFunc(s.postEvent)))
	mux.Handle("POST /v1/events/cloudevents", s.auth(http.HandlerFunc(s.postCloudEvent)))
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	return mux
}

// auth enforces the bearer token using a constant-time comparison.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeError(w, http.StatusUnauthorized,
				"missing or invalid bearer token; set Authorization: Bearer <token>")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// healthz reports process liveness only. It must not touch the database: a failing
// liveness probe restarts the pod, and restarting will not fix a database outage.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports whether this instance can serve traffic, which requires the database.
// A failing readiness probe removes the pod from the load balancer without killing it.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Pool().Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unreachable: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type eventRequest struct {
	Tenant         string         `json:"tenant"`
	Source         string         `json:"source"`
	Type           string         `json:"type"`
	Subject        string         `json:"subject"`
	Actor          string         `json:"actor"`
	Title          string         `json:"title"`
	OccurredAt     time.Time      `json:"occurred_at"`
	CorrelationRef string         `json:"correlation_ref"`
	IdempotencyKey string         `json:"idempotency_key"`
	Attributes     map[string]any `json:"attributes"`
}

func (s *Server) postEvent(w http.ResponseWriter, r *http.Request) {
	var req eventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error()+
			"; expected an object with source, type, subject and occurred_at")
		return
	}
	now := s.clock()
	tenantID := req.Tenant
	if tenantID == "" {
		tenantID = DefaultTenant
	}
	ev := core.Event{
		ID:             core.NewULID(now, s.entropy),
		Tenant:         tenantID,
		IdempotencyKey: req.IdempotencyKey,
		SchemaVersion:  core.CurrentSchemaVersion,
		Source:         req.Source,
		Type:           req.Type,
		Subject:        req.Subject,
		Actor:          req.Actor,
		Title:          req.Title,
		OccurredAt:     req.OccurredAt,
		ReceivedAt:     now,
		CorrelationRef: req.CorrelationRef,
		Attributes:     req.Attributes,
	}
	s.append(w, r, ev)
}

func (s *Server) append(w http.ResponseWriter, r *http.Request, ev core.Event) {
	stored, created, err := s.store.AppendEvent(r.Context(), ev)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"id": stored.ID, "ref": core.RefFor(stored), "created": created,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
