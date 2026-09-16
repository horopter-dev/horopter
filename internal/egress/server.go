// Package egress is the read-side daemon: it serves queries and renders them through
// vendor adapters.
//
// It contains no write code by design: no code here calls AppendEvent or
// RebuildProjection, and Routes exposes no path that reaches them. That is a
// discipline this package's code observes, not a privilege boundary — Server holds a
// full *postgres.Store, which exposes those same write methods, so a compromise of the
// egress process itself still has a write-capable Store sitting in memory. What
// splitting into two binaries buys is that a bug specific to the write path is only
// reachable through ingress's routes, and a bug specific to the read path only through
// egress's.
package egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/yo61/horopter/internal/adapter/grafana"
	"github.com/yo61/horopter/internal/adapter/otlp"
	"github.com/yo61/horopter/internal/core"
	"github.com/yo61/horopter/internal/store/postgres"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// DefaultTenant is used when a request omits one.
const DefaultTenant = "default"

// MaxLimit bounds every query's result set, by default and as a hard cap on any
// caller-supplied "limit" parameter. Egress has no auth in v0.1, so without a cap an
// unauthenticated caller could exhaust the database or exfiltrate an entire tenant's
// event log or change history in a single request.
const MaxLimit = 1000

// TruncatedHeader is set to "true" on a response whose result set was cut short by the
// limit, and is absent otherwise. Without it a caller cannot tell a result of exactly
// MaxLimit rows from one where more exist — and since the ordering is start_at ASC,
// truncation discards the most RECENT rows, which during an incident are exactly the ones
// that matter. Silently returning the cap as if it were the whole answer is the failure
// this closes.
//
// A header rather than a flag in the body: two of the four read endpoints render
// vendor-owned response shapes (Grafana annotations, OTLP spans) that cannot carry a
// Horopter envelope, so a header is the only signal that works uniformly across all four
// without breaking any of them.
const TruncatedHeader = "X-Horopter-Truncated"

// MaxAnnotationBodyBytes caps a Grafana annotation POST body. The documented shape —
// a time range plus a short "rangeRaw" and "annotation" echo — is a few hundred bytes;
// 64 KiB is two orders of magnitude of headroom for an unusually long saved-query
// name, and nowhere near enough to be a memory lever on this unauthenticated route.
// Until this batch every egress route was a GET, so caller input was bounded by
// http.Server.MaxHeaderBytes; this is the first route that reads a body at all.
const MaxAnnotationBodyBytes = 1 << 16

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
	mux.HandleFunc("POST /adapters/grafana/annotations", s.grafanaAnnotationsPOST)
	mux.HandleFunc("POST /adapters/grafana/{tenant}/annotations", s.grafanaAnnotationsPOST)
	mux.HandleFunc("GET /adapters/otlp/spans", s.otlpSpans)
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
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Pool().Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unreachable: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// tenantOf resolves the tenant and reports which mechanism supplied it.
//
// The source is recorded because the path-scoped Grafana route exists on the finding
// that both of Grafana's access modes discard a query string baked into a configured
// datasource URL. If that is wrong, this attribute reads "default" on every
// Grafana-originated request in production — visible at once, rather than surfacing
// later as quietly wrong data.
//
// The results are NAMED because they are two bare same-typed strings: nothing catches a
// transposition at either end, which is the same reason replayProjection names its two
// ints in internal/store/postgres.
func tenantOf(r *http.Request) (tenantID, source string) {
	if v := r.PathValue("tenant"); v != "" {
		return v, "path"
	}
	if v := r.URL.Query().Get("tenant"); v != "" {
		return v, "query"
	}
	return DefaultTenant, "default"
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

// minEpochMillis and maxEpochMillis bound a Grafana-style epoch-millisecond parameter.
//
// The ceiling is core.OpenSentinel, the literal migration 0002 generates for an open
// change's end_effective: nothing in the projection can extend past it, so a window
// reaching beyond it can contain nothing a shorter one does not. The floor is the Unix
// epoch, which is where this transport's own zero is.
//
// The bound belongs at the parser, because nothing downstream of it reports the problem.
// Without it any int64 was accepted: ?to=9223372036854775807 became a year-292278994
// time.Time, and pgx encodes timestamptz as microseconds since 2000-01-01 in an int64, so
// that value OVERFLOWS and wraps. Measured against this stack (pgx v5, Postgres 18) it
// binds as 1969-12-31T23:59:59.999Z — a caller asking for everything up to the end of
// time is silently given a window ending before the Unix epoch, and answered 200 with an
// empty list. No error is raised anywhere, so relaying store errors verbatim never comes
// into it and there is nothing for an operator to notice; the caller simply gets a
// confident wrong answer. A 400 naming the parameter is the only place that can be
// caught.
var (
	minEpochMillis = time.Unix(0, 0).UTC().UnixMilli()
	maxEpochMillis = core.OpenSentinel.UnixMilli()
)

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
	if ms < minEpochMillis || ms > maxEpochMillis {
		return time.Time{}, fmt.Errorf(
			"query parameter %q=%d is out of range; epoch milliseconds must be between "+
				"%d (%s) and %d (%s)",
			name, ms,
			minEpochMillis, time.UnixMilli(minEpochMillis).UTC().Format(time.RFC3339),
			maxEpochMillis, time.UnixMilli(maxEpochMillis).UTC().Format(time.RFC3339))
	}
	return time.UnixMilli(ms).UTC(), nil
}

// limitParam parses the optional "limit" query parameter. Absent, it defaults to
// MaxLimit; present, it is capped at MaxLimit rather than rejected (a caller asking
// for more than the cap gets the cap, not an error), and rejected only when it is not
// a positive integer.
func limitParam(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return MaxLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf(
			"query parameter \"limit\"=%q is not a number; expected an integer "+
				"between 1 and %d", raw, MaxLimit)
	}
	if n < 1 {
		return 0, fmt.Errorf(
			"query parameter \"limit\"=%d is out of range; expected an integer "+
				"between 1 and %d", n, MaxLimit)
	}
	if n > MaxLimit {
		return MaxLimit, nil
	}
	return n, nil
}

// window bounds a query in both time and result-set size.
type window struct {
	from, to time.Time
	limit    int
}

// newWindow assembles a validated window, and is the single place from and to are
// checked against each other. All three parsers below build their result through it —
// RFC3339 query parameters, Grafana's epoch milliseconds, and Grafana's RFC3339 POST
// body — so a fourth transport cannot be added without inheriting the rule.
//
// An inverted window is the caller's error, not an empty result. The overlap predicate
// is "start_at <= :to AND end_effective >= :from", which for from > to selects the
// changes SPANNING the interval the caller inverted: a non-empty answer bearing no
// relation to the question asked, and indistinguishable from a correct one.
func newWindow(from, to time.Time, limit int) (window, error) {
	if from.After(to) {
		return window{}, fmt.Errorf(
			"query window from=%s is after to=%s; give the range earliest first",
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	}
	return window{from: from, to: to, limit: limit}, nil
}

// windowFromRFC3339 reads from/to as RFC3339 and limit as an optional bounded
// integer, matching the neutral API and the OTLP adapter's parameter contract.
func windowFromRFC3339(r *http.Request) (window, error) {
	from, err := rfc3339Param(r, "from")
	if err != nil {
		return window{}, err
	}
	to, err := rfc3339Param(r, "to")
	if err != nil {
		return window{}, err
	}
	limit, err := limitParam(r)
	if err != nil {
		return window{}, err
	}
	return newWindow(from, to, limit)
}

// windowFromEpochMillis reads from/to as epoch milliseconds and limit as an optional
// bounded integer: the neutral epoch-millisecond query-parameter contract. This is
// not Grafana's own contract — windowFromAnnotationBody, reading RFC3339 out of a
// POST body, is the parser grafana/simple-json-datasource — an archived Grafana
// datasource plugin, but the only one that speaks this transport — actually drives.
func windowFromEpochMillis(r *http.Request) (window, error) {
	from, err := epochMillisParam(r, "from")
	if err != nil {
		return window{}, err
	}
	to, err := epochMillisParam(r, "to")
	if err != nil {
		return window{}, err
	}
	limit, err := limitParam(r)
	if err != nil {
		return window{}, err
	}
	return newWindow(from, to, limit)
}

// grafanaAnnotationRequest is the JSON body grafana/simple-json-datasource — an
// archived Grafana datasource plugin, but the only one that implements this
// transport — POSTs to an annotations endpoint. It also sends "rangeRaw" (the
// panel's relative range, e.g. "now-1h") and "annotation" (the query config);
// this adapter's contract needs neither, only the resolved range.
type grafanaAnnotationRequest struct {
	Range struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"range"`
}

// windowFromAnnotationBody reads a Grafana annotation POST body's time range.
// This is the third window parser alongside windowFromRFC3339 and
// windowFromEpochMillis, feeding the same render path — Grafana's POST body
// carries RFC3339 timestamps with fractional seconds (e.g.
// "2016-04-15T13:44:39.070Z"), not the epoch milliseconds its GET query
// parameters use: a different contract from the same vendor, not a
// duplicate of one already handled. time.Parse(time.RFC3339, …) already
// accepts the fractional-second form, so no new parsing helper is needed for
// it.
func windowFromAnnotationBody(r *http.Request) (window, error) {
	var body grafanaAnnotationRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return window{}, fmt.Errorf("request body is not valid JSON: %w", err)
	}
	from, err := time.Parse(time.RFC3339, body.Range.From)
	if err != nil {
		return window{}, fmt.Errorf("range.from=%q is not RFC3339: %w", body.Range.From, err)
	}
	to, err := time.Parse(time.RFC3339, body.Range.To)
	if err != nil {
		return window{}, fmt.Errorf("range.to=%q is not RFC3339: %w", body.Range.To, err)
	}
	limit, err := limitParam(r)
	if err != nil {
		return window{}, err
	}
	return newWindow(from, to, limit)
}

func (s *Server) changesFor(r *http.Request, win window) ([]core.Change, bool, error) {
	tenantID, source := tenantOf(r)
	trace.SpanFromContext(r.Context()).SetAttributes(
		attribute.String("horopter.tenant", tenantID),
		attribute.String("horopter.tenant.source", source),
	)
	return s.store.QueryChanges(r.Context(), postgres.ChangeQuery{
		Tenant:  tenantID,
		From:    win.from,
		To:      win.to,
		Subject: r.URL.Query().Get("subject"),
		Type:    r.URL.Query().Get("type"),
		Status:  r.URL.Query().Get("status"),
		Limit:   win.limit,
	})
}

func (s *Server) queryChanges(w http.ResponseWriter, r *http.Request) {
	win, err := windowFromRFC3339(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	trace.SpanFromContext(r.Context()).SetAttributes(
		attribute.String("horopter.adapter", "neutral"))
	changes, truncated, err := s.changesFor(r, win)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	markTruncated(w, truncated)
	writeJSON(w, http.StatusOK, changesToJSON(changes))
}

// listEvents returns the raw event log for a tenant, optionally scoped to one change's
// ref, for audit and export. It reads only — no write path exists here.
//
// "ref" is a projection ref, matched the way core.RefFor forms one: a correlated event's
// correlation_ref, or an uncorrelated point change's own event id. See the store's
// ListEvents for why the id arm is restricted to rows carrying no correlation_ref.
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	limit, err := limitParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tenantID, source := tenantOf(r)
	trace.SpanFromContext(r.Context()).SetAttributes(
		attribute.String("horopter.adapter", "neutral"),
		attribute.String("horopter.tenant", tenantID),
		attribute.String("horopter.tenant.source", source),
	)
	events, truncated, err := s.store.ListEvents(r.Context(), postgres.EventQuery{
		Tenant: tenantID,
		Ref:    r.URL.Query().Get("ref"),
		Limit:  limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	markTruncated(w, truncated)
	writeJSON(w, http.StatusOK, eventsToJSON(events))
}

func (s *Server) grafanaAnnotations(w http.ResponseWriter, r *http.Request) {
	win, err := windowFromEpochMillis(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.render(w, r, win, "grafana", s.grafana.Render)
}

// grafanaAnnotationsPOST serves the same annotations the GET route does, for the
// caller that needs it: grafana/simple-json-datasource — an archived Grafana
// datasource plugin, but the only one that speaks this transport — queries
// annotations with a POST carrying a JSON body, not a GET with query parameters, so
// without this route no such Grafana instance could call this adapter at all. The
// renderer is unchanged — grafana.Renderer.Render is identical either way — only how
// the time window, and now the tenant, arrive differs.
//
// Both the tenant-less route and the path-scoped "/{tenant}/annotations" route (see
// Routes) reach this same handler: tenantOf reads the path wildcard when the matched
// route registered one and falls back to DefaultTenant when it did not, so one
// handler serves both without a branch here.
func (s *Server) grafanaAnnotationsPOST(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxAnnotationBodyBytes)
	win, err := windowFromAnnotationBody(r)
	if err != nil {
		writeAnnotationBodyError(w, err)
		return
	}
	s.render(w, r, win, "grafana", s.grafana.Render)
}

// writeAnnotationBodyError answers a POST body that could not be read. An
// over-sized body is 413, not 400: it may be well-formed JSON that is merely too
// large, and http.MaxBytesReader surfaces the cap as a read error indistinguishable
// from malformed JSON unless checked explicitly — see ingress's writeBodyError for
// the same reasoning applied to the write side of this project.
func writeAnnotationBodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"request body exceeds the %d-byte limit", tooLarge.Limit))
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

func (s *Server) otlpSpans(w http.ResponseWriter, r *http.Request) {
	win, err := windowFromRFC3339(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.render(w, r, win, "otlp", s.otlp.Render)
}

func (s *Server) render(
	w http.ResponseWriter, r *http.Request, win window, adapter string,
	render func([]core.Change, time.Time) (any, error),
) {
	// Set before the query, not after it: which adapter a request was for is known at
	// entry, and a failing query is exactly when you need to know whether it was Grafana
	// or OTLP asking. Recording it only on success left every failed read unattributed.
	trace.SpanFromContext(r.Context()).SetAttributes(
		attribute.String("horopter.adapter", adapter))

	changes, truncated, err := s.changesFor(r, win)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	payload, err := render(changes, s.clock())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	markTruncated(w, truncated)
	writeJSON(w, http.StatusOK, payload)
}

// changesToJSON renders changes in neutral terms. No vendor field names, no epoch
// units — those belong to the adapters.
//
// "start" is the answer to "when did this change start"; first_seen_at and started_at are
// the two facts it is derived from. Both are emitted because the point of recording them
// separately is that first_seen_at < started_at — an event claiming to predate the
// change's declared start — becomes a queryable anomaly, and it cannot be queried if the
// read side never emits it. started_at is omitted while it is NULL, the way "end" is
// omitted while the change is open: absent means "no producer has asserted a start yet",
// which is not the same fact as any timestamp would be.
func changesToJSON(changes []core.Change) []map[string]any {
	out := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		m := map[string]any{
			"ref": c.Ref, "subject": c.Subject, "type": c.Type, "title": c.Title,
			"actor": c.Actor, "start": c.Start.UTC().Format(time.RFC3339Nano),
			"first_seen_at": c.FirstSeenAt.UTC().Format(time.RFC3339Nano),
			"status":        string(c.Status), "attributes": c.Attributes,
			"last_event_id": c.LastEventID,
		}
		if c.StartedAt != nil {
			m["started_at"] = c.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		if !c.IsOpen() {
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

// markTruncated sets TruncatedHeader when the result set was cut short. It must run
// before writeJSON: WriteHeader flushes the header block, and anything set after it is
// silently dropped.
func markTruncated(w http.ResponseWriter, truncated bool) {
	if truncated {
		w.Header().Set(TruncatedHeader, "true")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
