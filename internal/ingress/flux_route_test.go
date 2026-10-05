package ingress_test

import (
	"context"
	"crypto/sha1" //nolint:gosec // a test fixture's fake Git revision, not a security use
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/store/postgres"
)

func postFlux(t *testing.T, h http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doPost(t, h, "/v1/events/flux", "application/json", key, body)
}

// uniqueRevision is a Git revision no other test run has used: the database is shared and
// never truncated, and Flux keys are built from the revision.
func uniqueRevision(t *testing.T) string {
	t.Helper()
	sum := sha1.Sum([]byte(idemKey(t, "rev"))) //nolint:gosec // see the import
	return "refs/heads/main@sha1:" + hex.EncodeToString(sum[:])
}

func fluxBody(kind, name, reason, severity, revision string, at time.Time) string {
	return fmt.Sprintf(`{"involvedObject":{"kind":%q,"namespace":"flux-system","name":%q},
		"severity":%q,"timestamp":%q,"message":"m","reason":%q,
		"metadata":{"revision":%q}}`,
		kind, name, severity, at.Format(time.RFC3339), reason, revision)
}

type fluxResponse struct {
	Recorded int    `json:"recorded"`
	Created  int    `json:"created"`
	Ignored  string `json:"ignored"`
}

func decodeFlux(t *testing.T, rec *httptest.ResponseRecorder) fluxResponse {
	t.Helper()
	var got fluxResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), rec.Body.String())
	return got
}

func changeRow(t *testing.T, s *postgres.Store, tenant core.TenantID, ref string) (
	status string, start time.Time, end *time.Time,
) {
	t.Helper()
	require.NoError(t, s.Pool().QueryRow(context.Background(),
		`SELECT status, start_at, end_at FROM change WHERE tenant = $1 AND ref = $2`,
		tenant.String(), ref).Scan(&status, &start, &end))
	return status, start, end
}

func countKey(t *testing.T, s *postgres.Store, key string) int {
	t.Helper()
	var n int
	require.NoError(t, s.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM event WHERE idempotency_key = $1`, key).Scan(&n))
	return n
}

func TestFluxRolloutIsOneChangeEndedByTheFirstSuccess(t *testing.T) {
	f := newIngressFixture(t)
	tenant, key := seedCredential(t, f.store, "flux", "")
	r, ks := uniqueRevision(t), "apps-"+uniqueToken()
	t0 := time.Date(2026, 10, 5, 14, 40, 2, 0, time.UTC)
	t1, t2 := t0.Add(7*time.Second), t0.Add(time.Hour)

	start := postFlux(t, f.srv, key, fluxBody("GitRepository", "flux-system-"+uniqueToken(),
		"NewArtifact", "info", r, t0))
	require.Equal(t, http.StatusCreated, start.Code, start.Body.String())
	end := postFlux(t, f.srv, key, fluxBody("Kustomization", ks,
		"ReconciliationSucceeded", "info", r, t1))
	require.Equal(t, http.StatusCreated, end.Code, end.Body.String())
	noop := postFlux(t, f.srv, key, fluxBody("Kustomization", ks,
		"ReconciliationSucceeded", "info", r, t2))
	require.Equal(t, http.StatusOK, noop.Code, noop.Body.String())
	assert.Equal(t, fluxResponse{Recorded: 1, Created: 0}, decodeFlux(t, noop))

	status, startAt, endAt := changeRow(t, f.store, tenant, "flux/"+r)
	assert.Equal(t, "completed", status)
	assert.True(t, startAt.Equal(t0), "the change starts at NewArtifact, got %v", startAt)
	require.NotNil(t, endAt)
	assert.True(t, endAt.Equal(t1), "the first success ends it, not the no-op, got %v", *endAt)
}

func TestFluxFailureStoresTheChangeEndAndAPoint(t *testing.T) {
	f := newIngressFixture(t)
	tenant, key := seedCredential(t, f.store, "flux", "")
	r, ks := uniqueRevision(t), "apps-"+uniqueToken()
	at := time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)

	rec := postFlux(t, f.srv, key, fluxBody("Kustomization", ks, "HealthCheckFailed",
		"error", r, at))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, fluxResponse{Recorded: 2, Created: 2}, decodeFlux(t, rec))

	status, _, _ := changeRow(t, f.store, tenant, "flux/"+r)
	assert.Equal(t, "failed", status)
	k := "Kustomization/flux-system/" + ks + "/" + r + "/HealthCheckFailed"
	assert.Equal(t, 1, countKey(t, f.store, k))
	assert.Equal(t, 1, countKey(t, f.store, k+"/point"))
}

// TestFluxRetryAfterAPartialFailureStoresTheMissingEvent stands in for a failure whose second
// append hit a 503: the change end is already stored, here through the native route under the
// same key, and the retried Flux event must store only the point.
func TestFluxRetryAfterAPartialFailureStoresTheMissingEvent(t *testing.T) {
	f := newIngressFixture(t)
	_, key := seedCredential(t, f.store, "flux", "")
	r, ks := uniqueRevision(t), "apps-"+uniqueToken()
	k := "Kustomization/flux-system/" + ks + "/" + r + "/HealthCheckFailed"
	first := post(t, f.srv, key, fmt.Sprintf(`{"type":"flux.kustomization.failed",
		"subject":"Kustomization/flux-system/%s","occurred_at":"2026-10-05T15:00:00Z",
		"lifecycle":"failed","correlation_ref":%q,"idempotency_key":%q}`, ks, r, k))
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())

	rec := postFlux(t, f.srv, key, fluxBody("Kustomization", ks, "HealthCheckFailed",
		"error", r, time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, fluxResponse{Recorded: 2, Created: 1}, decodeFlux(t, rec))
	assert.Equal(t, 1, countKey(t, f.store, k+"/point"))
}

func TestFluxIgnoredEventStoresNothing(t *testing.T) {
	f := newIngressFixture(t)
	_, key := seedCredential(t, f.store, "flux", "")
	ks := "apps-" + uniqueToken()
	rec := postFlux(t, f.srv, key, fluxBody("Kustomization", ks, "DependencyNotReady",
		"info", uniqueRevision(t), time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeFlux(t, rec)
	assert.Equal(t, 0, got.Recorded)
	assert.Contains(t, got.Ignored, "DependencyNotReady")
	assert.Equal(t, 0, eventCountBySubject(t, f.store, "Kustomization/flux-system/"+ks))
}

func TestFluxRejectsMalformedAndIncompleteBodies(t *testing.T) {
	f := newIngressFixture(t)
	_, key := seedCredential(t, f.store, "flux", "")
	for name, body := range map[string]string{
		"not JSON": "not json",
		"no timestamp": `{"involvedObject":{"kind":"Kustomization","namespace":"flux-system",
			"name":"apps"},"severity":"info","reason":"ReconciliationSucceeded"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := postFlux(t, f.srv, key, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
}

func TestFluxRouteRequiresACredential(t *testing.T) {
	f := newIngressFixture(t)
	rec := postFlux(t, f.srv, "", fluxBody("Kustomization", "apps",
		"ReconciliationSucceeded", "info", uniqueRevision(t), time.Now()))
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
}

func TestFluxEventsCarryTheCredentialsSourceAndLocus(t *testing.T) {
	f := newIngressFixture(t)
	_, key := seedCredential(t, f.store, "flux", "")
	r, ks := uniqueRevision(t), "apps-"+uniqueToken()
	rec := postFlux(t, f.srv, key, fluxBody("Kustomization", ks, "ReconciliationSucceeded",
		"info", r, time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var source, locus, ref string
	require.NoError(t, f.store.Pool().QueryRow(context.Background(),
		`SELECT source, locus, correlation_ref FROM event WHERE idempotency_key = $1`,
		"Kustomization/flux-system/"+ks+"/"+r+"/ReconciliationSucceeded").
		Scan(&source, &locus, &ref))
	assert.Equal(t, credentialSource, source)
	assert.Equal(t, "flux", locus)
	assert.Equal(t, "flux/"+r, ref)
}
