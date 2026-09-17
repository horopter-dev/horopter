package egress

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// The outcomes a gated read ends in, recorded on its server span as horopter.admission.outcome.
const (
	outcomeAdmitted        = "admitted"
	outcomeShedConcurrency = "shed_concurrency"
)

// The gates a read can be shed at, as the "gate" log field names them.
const gateConcurrency = "concurrency"

// afterAdmissionHook is nil in production. Tests set it to hold an admitted read in flight so
// the next read finds the concurrency limit full.
var afterAdmissionHook func()

// readState is what admission learned about one read, recorded on the read's server span when
// the read finishes.
type readState struct {
	outcome string
	waited  time.Duration
}

// admit gates a read route on the concurrency limit. A read that gets no permit within
// Limits.AdmissionWait is shed with 503; an admitted read holds its permit until its response
// has been written.
func (s *Server) admit(read http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &readState{outcome: outcomeAdmitted}
		defer state.record(r.Context())

		started := time.Now()
		waitCtx, cancel := context.WithTimeout(r.Context(), s.limits.AdmissionWait)
		permit, err := s.limiter.Acquire(waitCtx)
		cancel()
		state.waited = time.Since(started)
		if err != nil {
			state.outcome = outcomeShedConcurrency
			s.shed(w, r, state, gateConcurrency,
				"limit", s.limiter.Limit(), "in_flight", s.limiter.InFlight())
			return
		}
		defer permit.Release(false)

		if afterAdmissionHook != nil {
			afterAdmissionHook()
		}
		read(w, r)
	})
}

// shed answers a read refused at gate with 503 and Retry-After, and logs it at WARN with the
// gate's figures. Every shed read is logged: shedding is driven by database capacity, far below
// the rate at which the handler can write a log line, and a volumetric flood is expected to be
// stopped in front of Horopter.
func (s *Server) shed(
	w http.ResponseWriter, r *http.Request, state *readState, gate string, figures ...any,
) {
	tenantID, _ := tenantOf(r)
	retryAfter := retryAfterSeconds(s.limits.AdmissionWait)
	fields := []any{"route", r.Pattern, "tenant", tenantID, "gate", gate,
		"waited_ms", state.waited.Milliseconds(), "retry_after_s", retryAfter}
	slog.WarnContext(r.Context(), "read shed", append(fields, figures...)...)
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeError(w, http.StatusServiceUnavailable,
		"egress is at capacity; retry after the interval in Retry-After")
}

// retryAfterSeconds is the Retry-After a shed read is given: the admission wait rounded up to
// whole seconds, and at least one.
func retryAfterSeconds(wait time.Duration) int {
	return max(1, int((wait+time.Second-1)/time.Second))
}

// record sets what admission decided on the read's server span.
func (st *readState) record(ctx context.Context) {
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String("horopter.admission.outcome", st.outcome),
		attribute.Int64("horopter.admission.wait_ms", st.waited.Milliseconds()),
	)
}
