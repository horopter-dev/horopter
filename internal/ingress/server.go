// Package ingress is the write-side daemon; it holds a full Store but never calls a read method.
package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/logging"
	"horopter.dev/horopter/internal/store/postgres"
)

// MaxBodyBytes caps a request body: both readers consume the whole body before validating it.
const MaxBodyBytes = 1 << 20

// Server is the ingress HTTP surface.
type Server struct {
	store   *postgres.Store
	clock   func() time.Time
	entropy io.Reader
	authn   *auth.Authenticator
	// allowLocusPrefixedRef lifts resolveRef's guard; the zero value keeps it.
	allowLocusPrefixedRef bool
}

// New returns an ingress Server, refusing a nil authenticator: every write route needs one.
func New(
	s *postgres.Store, clock func() time.Time, entropy io.Reader, authn *auth.Authenticator,
	opts ...Option,
) (*Server, error) {
	if authn == nil {
		return nil, errors.New(
			"ingress requires an authenticator; build one with auth.New over a credential " +
				"store — a nil authenticator would panic on the first request rather than " +
				"reject it")
	}
	srv := &Server{store: s, clock: clock, entropy: entropy, authn: authn}
	for _, opt := range opts {
		opt(srv)
	}
	return srv, nil
}

// WithAllowLocusPrefixedRef lifts the guard against a ref starting with the credential's locus.
func WithAllowLocusPrefixedRef(allow bool) Option {
	return func(s *Server) { s.allowLocusPrefixedRef = allow }
}

// Option configures a Server at construction.
type Option func(*Server)

// Routes returns the handler for every ingress endpoint; only the probes are unauthenticated.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/events", s.authn.Require(auth.CapAppend)(http.HandlerFunc(s.postEvent)))
	mux.Handle("POST /v1/events/cloudevents",
		s.authn.Require(auth.CapAppend)(http.HandlerFunc(s.postCloudEvent)))
	mux.Handle("POST /v1/admin/refold",
		s.authn.Require(auth.CapAdmin)(http.HandlerFunc(s.postRefold)))
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	// Requests sits inside otelhttp to log with its span; otelhttp ignores the empty operation.
	return otelhttp.NewHandler(logging.Requests(mux), "")
}

// Must not touch the database: restarting the pod will not fix a database outage.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// pgx's error names the DB user, name, host and port, so it is logged, not relayed.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Pool().Ping(r.Context()); err != nil {
		slog.ErrorContext(r.Context(), "readiness probe failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type eventRequest struct {
	// RawMessage, not string: a present-but-null key must be refused like any other value.
	Tenant         json.RawMessage `json:"tenant"`
	Source         json.RawMessage `json:"source"`
	Type           string          `json:"type"`
	Subject        string          `json:"subject"`
	Actor          string          `json:"actor"`
	Title          string          `json:"title"`
	OccurredAt     time.Time       `json:"occurred_at"`
	CorrelationRef string          `json:"correlation_ref"`
	IdempotencyKey string          `json:"idempotency_key"`
	Attributes     map[string]any  `json:"attributes"`
	Lifecycle      string          `json:"lifecycle"`
}

func recordTenant(ctx context.Context, tenantID string) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("horopter.tenant", tenantID))
}

// An empty subject never matches a pattern, though path.Match's "*" would match it.
func subjectAllowed(pattern, subject string) bool {
	if pattern == "" {
		return true
	}
	if subject == "" {
		return false
	}
	ok, err := path.Match(pattern, subject)
	if err != nil {
		// A malformed pattern is an issuing defect; refuse it like a mismatch.
		slog.Error("credential subject_pattern is not a valid pattern",
			"pattern", pattern, "error", err)
		return false
	}
	return ok
}

// Validate before composing with locus: "github/ deploy-1" would hide the leading space.
func resolveRef(locus core.Locus, ref string, allowLocusPrefix bool) (core.Ref, error) {
	if ref == "" {
		return core.Ref{}, nil
	}
	if err := auth.ValidateRef(ref); err != nil {
		return core.Ref{}, err
	}
	if !allowLocusPrefix {
		if first, _, found := strings.Cut(ref, "/"); found && first == locus.String() {
			return core.Ref{}, fmt.Errorf(
				"correlation_ref %q already starts with this credential's locus %q, so "+
					"storing it would compose %q and address a different change than the "+
					"one you meant; echo the append response's \"correlation_ref\" rather "+
					"than its \"ref\", which is already prefixed. Set %s=true if your refs "+
					"genuinely begin %q",
				ref, locus, locus.String()+"/"+ref, allowLocusPrefixedRefVar,
				locus.String()+"/")
		}
	}
	composed, err := core.NewRef(locus, ref)
	if err != nil {
		return core.Ref{}, err
	}
	if len(composed.String()) > auth.MaxRefBytes {
		return core.Ref{}, fmt.Errorf("ref %q is %d bytes once prefixed with locus %q; the "+
			"limit is %d", ref, len(composed.String()), locus, auth.MaxRefBytes)
	}
	return composed, nil
}

func subjectForbidden(w http.ResponseWriter, pattern, subject string) {
	writeError(w, http.StatusForbidden, fmt.Sprintf(
		"subject %q is outside this credential's scope %q", subject, pattern))
}

func (s *Server) postEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	var req eventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBodyError(w, err, "malformed JSON body: "+err.Error()+
			"; expected an object with type, subject and occurred_at")
		return
	}
	if req.Tenant != nil {
		writeError(w, http.StatusBadRequest,
			"tenant is derived from the credential; remove it from the request body")
		return
	}
	if req.Source != nil {
		writeError(w, http.StatusBadRequest,
			"source is derived from the credential; remove it from the request body")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if !subjectAllowed(p.SubjectPattern, req.Subject) {
		subjectForbidden(w, p.SubjectPattern, req.Subject)
		return
	}
	ref, err := resolveRef(core.LocusFromRow(p.Locus), req.CorrelationRef, s.allowLocusPrefixedRef)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	now := s.clock()
	recordTenant(r.Context(), p.Tenant.String())
	ev := core.Event{
		ID:             core.NewULID(now, s.entropy),
		Tenant:         p.Tenant.String(),
		IdempotencyKey: req.IdempotencyKey,
		SchemaVersion:  core.CurrentSchemaVersion,
		Source:         p.Source,
		Type:           req.Type,
		Subject:        req.Subject,
		Actor:          req.Actor,
		Title:          req.Title,
		OccurredAt:     req.OccurredAt,
		ReceivedAt:     now,
		CorrelationRef: ref,
		Attributes:     req.Attributes,
		Origin:         p.Origin,
		Locus:          core.LocusFromRow(p.Locus),
		Lifecycle:      core.Lifecycle(req.Lifecycle),
	}
	s.append(w, r, ev)
}

func (s *Server) append(w http.ResponseWriter, r *http.Request, ev core.Event) {
	annotateEvent(r.Context(), ev)
	stored, created, err := s.store.AppendEvent(r.Context(), ev)
	if err != nil {
		// A correlated event's ref is the only handle on it apart from the event itself.
		if !ev.CorrelationRef.IsZero() {
			logging.Annotate(r.Context(), slog.String("ref", ev.CorrelationRef.String()))
		}
		appendFailed(w, r, ev, err)
		return
	}
	// The stored event, not ev: a replayed idempotency key returns the original.
	logging.Annotate(r.Context(),
		slog.String("ref", core.RefFor(stored).String()),
		slog.String("subject", stored.Subject),
		slog.String("type", stored.Type))
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"id": stored.ID, "ref": core.RefFor(stored).String(),
		"correlation_ref": amendHandle(stored), "created": created,
	})
}

// Never attributes: they are operator-owned and may hold anything.
func annotateEvent(ctx context.Context, ev core.Event) {
	attrs := []slog.Attr{slog.String("subject", ev.Subject), slog.String("type", ev.Type)}
	if ev.IdempotencyKey != "" {
		attrs = append(attrs, slog.String("idempotency_key", ev.IdempotencyKey))
	}
	logging.Annotate(ctx, attrs...)
}

// RefFor composed with its inverse ProducerRef: the value a producer echoes to amend.
func amendHandle(ev core.Event) string { return core.RefFor(ev).ProducerRef() }

// Only validation failures are 400: producers never retry a 4xx, so the rest are 503.
func appendFailed(w http.ResponseWriter, r *http.Request, ev core.Event, err error) {
	if errors.Is(err, core.ErrInvalidEvent) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	slog.ErrorContext(r.Context(), "append failed", "error", err,
		"tenant", ev.Tenant, "type", ev.Type, "subject", ev.Subject)
	writeError(w, http.StatusServiceUnavailable,
		"could not append the event; the store is temporarily unavailable — "+
			"this request may be retried")
}

// 413, not 400: the body may be well formed and merely too large.
func writeBodyError(w http.ResponseWriter, err error, malformedMsg string) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"request body exceeds the %d-byte limit; an event is a few kilobytes, so "+
				"reference a large attribute payload instead of inlining it",
			tooLarge.Limit))
		return
	}
	writeError(w, http.StatusBadRequest, malformedMsg)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
