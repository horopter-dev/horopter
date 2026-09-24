// Package ingress is the write-side daemon: it receives events, validates them, and
// appends them to the log, folding each into the projection in the same transaction.
//
// It contains no query code by design: no code here calls QueryChanges or ListEvents,
// and Routes exposes no path that reaches them. That is a discipline this package's
// code observes, not a privilege boundary — Server holds a full *postgres.Store, which
// exposes those same read methods, so a compromise of the ingress process itself still
// has a read-capable Store sitting in memory. What splitting into two binaries buys is
// that a bug specific to the read path is only reachable through egress's routes, and a
// bug specific to the write path only through ingress's.
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
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/store/postgres"
)

// MaxBodyBytes caps a request body. A fully populated event — every field set, a generous
// attribute map — is a few kilobytes, and a CloudEvents envelope adds a little more, so
// 1 MiB is three orders of magnitude of headroom for an unusually large attribute payload.
// It is also nowhere near enough to be a memory lever: both body readers here consume the
// whole body before anything is validated, so without a cap a single unauthenticated-
// adjacent request decides how much the process allocates.
const MaxBodyBytes = 1 << 20

// Server is the ingress HTTP surface.
type Server struct {
	store   *postgres.Store
	clock   func() time.Time
	entropy io.Reader
	authn   *auth.Authenticator
}

// New returns an ingress Server. clock and entropy are injected so handlers stay
// deterministic under test.
//
// A nil authenticator is refused here rather than left to callers to check: every write
// route is gated on it, so a Server built without one would either panic on the first
// request or, worse, need every caller to remember the guard itself.
func New(
	s *postgres.Store, clock func() time.Time, entropy io.Reader, authn *auth.Authenticator,
) (*Server, error) {
	if authn == nil {
		return nil, errors.New(
			"ingress requires an authenticator; build one with auth.New over a credential " +
				"store — a nil authenticator would panic on the first request rather than " +
				"reject it")
	}
	return &Server{store: s, clock: clock, entropy: entropy, authn: authn}, nil
}

// Routes returns the handler for every ingress endpoint. Probes are unauthenticated;
// everything else requires a credential granting the append capability.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/events", s.authn.Require(auth.CapAppend)(http.HandlerFunc(s.postEvent)))
	mux.Handle("POST /v1/events/cloudevents",
		s.authn.Require(auth.CapAppend)(http.HandlerFunc(s.postCloudEvent)))
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	// otelhttp gives every route a server span carrying the http.* semantic
	// conventions. It wraps the mux rather than each handler so a route added later is
	// covered without anyone remembering. With no provider installed it is a no-op.
	//
	// The operation argument is empty because it has no effect here. Without a
	// WithSpanNameFormatter option, otelhttp's default formatter ignores the operation
	// entirely and names the span from the request's method and route; passing the
	// binary name only made it look load-bearing. The binary name already reaches
	// backends as the service.name resource attribute.
	return otelhttp.NewHandler(mux, "")
}

// healthz reports process liveness only. It must not touch the database: a failing
// liveness probe restarts the pod, and restarting will not fix a database outage.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports whether this instance can serve traffic, which requires the database.
// A failing readiness probe removes the pod from the load balancer without killing it.
//
// The probe answers without a credential, and pgx formats a failed connection with the
// database user, database name, host and port, so the error goes to the server log and
// the body says only that the database is unreachable — appendFailed's rule, applied to
// the one route on this daemon that anyone can call.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Pool().Ping(r.Context()); err != nil {
		slog.ErrorContext(r.Context(), "readiness probe failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type eventRequest struct {
	// Tenant is json.RawMessage, not string, so a present-but-null "tenant":null is
	// distinguishable from an absent key: unmarshaling null into *string or string both
	// leave the zero value, indistinguishable from omission, and the rule is reject a
	// caller who sends the key at all, whatever its value — not just a non-empty one.
	Tenant         json.RawMessage `json:"tenant"`
	Source         string          `json:"source"`
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

// recordTenant puts the resolved tenant on the request's span. The tenant is the
// primary dimension every other question is sliced by, so it goes on the span that
// exists for every request rather than only on the ones that reach the store.
func recordTenant(ctx context.Context, tenantID string) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("horopter.tenant", tenantID))
}

// subjectAllowed reports whether subject is covered by pattern, an empty pattern meaning
// the credential is unrestricted.
//
// An empty subject never satisfies a non-empty pattern, checked before path.Match rather
// than left to it: path.Match's "*" matches an empty name too, so without this guard an
// absent subject could satisfy a narrowing pattern by that accident rather than because a
// producer actually posted one.
func subjectAllowed(pattern, subject string) bool {
	if pattern == "" {
		return true
	}
	if subject == "" {
		return false
	}
	ok, err := path.Match(pattern, subject)
	if err != nil {
		// A malformed pattern is an issuing defect, not this request's fault, and the
		// safe default is the same refusal a genuine mismatch gets.
		slog.Error("credential subject_pattern is not a valid pattern",
			"pattern", pattern, "error", err)
		return false
	}
	return ok
}

// resolveRef validates the producer's ref as given, composes it with the credential's
// locus, and caps the composed form — the form that is actually stored, so the length
// cap applies to what is stored rather than to what the producer sent. An empty ref
// yields the zero core.Ref: a point event has no correlation ref to namespace.
//
// This is the anti-corruption boundary between the wire and the domain. The composition
// itself belongs to core.NewRef; what stays here are the two rules about what a PRODUCER
// may send, which are auth's business rather than the type's — the ordering guard below
// and the byte cap.
//
// ValidateRef must run on ref BEFORE it is composed with locus, not after. Composing
// first can move a leading or trailing space away from the string's edge: " deploy-1"
// fails the whitespace check, but "github/ deploy-1" would not, since the space sits in
// the middle once prefixed. That check exists for exactly this producer mistake — an
// untrimmed ref posts a start under one ref and its completion under another, and
// nothing errors — so disabling it for the composed form defeats the reason it exists.
func resolveRef(locus core.Locus, ref string) (core.Ref, error) {
	if ref == "" {
		return core.Ref{}, nil
	}
	if err := auth.ValidateRef(ref); err != nil {
		return core.Ref{}, err
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

// subjectForbidden answers a subject the credential's subject_pattern does not cover.
func subjectForbidden(w http.ResponseWriter, pattern, subject string) {
	writeError(w, http.StatusForbidden, fmt.Sprintf(
		"subject %q is outside this credential's scope %q", subject, pattern))
}

func (s *Server) postEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	var req eventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBodyError(w, err, "malformed JSON body: "+err.Error()+
			"; expected an object with source, type, subject and occurred_at")
		return
	}
	if req.Tenant != nil {
		writeError(w, http.StatusBadRequest,
			"tenant is derived from the credential; remove it from the request body")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if !subjectAllowed(p.SubjectPattern, req.Subject) {
		subjectForbidden(w, p.SubjectPattern, req.Subject)
		return
	}
	ref, err := resolveRef(core.LocusFromRow(p.Locus), req.CorrelationRef)
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
		Source:         req.Source,
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
	stored, created, err := s.store.AppendEvent(r.Context(), ev)
	if err != nil {
		appendFailed(w, r, ev, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"id": stored.ID, "ref": core.RefFor(stored).String(), "created": created,
	})
}

// appendFailed answers a failed append, and the distinction it draws is the whole point
// of core.ErrInvalidEvent. Producers treat 4xx as "do not retry", so answering a
// connection failure, a commit failure or a lock timeout with 400 — as this handler used
// to do for every error alike — permanently loses every event posted during a database
// blip, with nothing in any log showing a hole exists. That is the worst failure a system
// of record can have.
//
// 400 is returned only for a validation failure, and its body carries the error text
// because that text describes the caller's own input back to them. Everything else is
// transient from the caller's point of view and gets 503: not 500, because 503 is what
// tells the producer to retry, which is what turns a lost event into a delayed one.
//
// The 503 body is generic on purpose. The underlying text names tables, constraint names
// and connection strings, and ingress's callers are not operators; the full detail goes to
// the server log instead, where an operator can correlate it with the request.
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

// writeBodyError answers a request body that could not be read.
//
// An over-sized body is 413, not 400: the body may be perfectly well formed and merely too
// large, and http.MaxBytesReader surfaces the cap as a read error, so reporting it as
// malformed JSON would send the caller hunting for a syntax error that is not there.
// Anything else is the caller's own malformed input, reported with malformedMsg.
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
