package ingress

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/logging"
	"horopter.dev/horopter/internal/store/postgres"
)

// refoldRequest is POST /v1/admin/refold's body. Tenant is for a global key.
type refoldRequest struct {
	Ref    string `json:"ref"`
	Tenant string `json:"tenant"`
}

// Takes the locks an append takes, so ingest to other refs carries on.
func (s *Server) postRefold(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	var req refoldRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBodyError(w, err, "malformed JSON body: "+err.Error()+
			`; expected {"ref": "<locus>/<ref>"}`)
		return
	}
	ref, err := core.ParseRef(req.Ref)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error()+
			"; give the stored form, as horo show prints it")
		return
	}
	// Annotated first, so every outcome's request line names the target change.
	logging.Annotate(r.Context(), slog.String("ref", ref.String()))
	tenantID, status, msg := s.refoldTenant(r, req.Tenant)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	recordTenant(r.Context(), tenantID)
	res, err := s.store.RefoldRef(r.Context(), tenantID, ref)
	if errors.Is(err, postgres.ErrRefNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no events for ref %q in this tenant", ref))
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "re-fold failed", "error", err,
			"tenant", tenantID, "ref", ref.String())
		writeError(w, http.StatusServiceUnavailable, "could not re-fold the change; the store "+
			"is temporarily unavailable — this request may be retried")
		return
	}
	logging.Annotate(r.Context(),
		slog.Int("refold_events", res.Events), slog.Any("refold_altered", res.Altered))
	writeJSON(w, http.StatusOK, map[string]any{
		"ref": ref.String(), "events": res.Events, "altered": res.Altered,
	})
}

// Egress's tenant rule; status is zero when the tenant resolved.
func (s *Server) refoldTenant(
	r *http.Request, named string,
) (tenantID string, status int, msg string) {
	p, _ := auth.PrincipalFrom(r.Context())
	if !p.IsGlobal() {
		if named != "" && named != p.Tenant.String() {
			return "", http.StatusBadRequest,
				"this key acts in its own tenant; remove tenant, or use a global key"
		}
		return p.Tenant.String(), 0, ""
	}
	if named == "" {
		return "", http.StatusBadRequest, `a global key must name the tenant: add "tenant"`
	}
	exists, err := s.store.TenantExists(r.Context(), named)
	if err != nil {
		slog.ErrorContext(r.Context(), "check tenant for re-fold", "error", err, "tenant", named)
		return "", http.StatusServiceUnavailable,
			"could not check the tenant; this request may be retried"
	}
	if !exists {
		return "", http.StatusNotFound, fmt.Sprintf("no tenant %q", named)
	}
	return named, 0, ""
}
