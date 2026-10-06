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
	// Flux keys are stored under the credential's locus, here "flux".
	k := "flux/Kustomization/flux-system/" + ks + "/" + r + "/HealthCheckFailed"
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
	// Flux keys are stored under the credential's locus, here "flux".
	k := "flux/Kustomization/flux-system/" + ks + "/" + r + "/HealthCheckFailed"
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
		"flux/Kustomization/flux-system/"+ks+"/"+r+"/ReconciliationSucceeded").
		Scan(&source, &locus, &ref))
	assert.Equal(t, credentialSource, source)
	assert.Equal(t, "flux", locus)
	assert.Equal(t, "flux/"+r, ref)
}

// TestANativeEventCannotPreemptAFluxKey pins the locus prefix on Flux keys. Keys are unique per
// tenant, not per locus, so without it a native producer in the tenant posting the bare
// Flux-shaped key, or a second cluster's identical objects, would make the Flux event a
// duplicate that is never folded.
func TestANativeEventCannotPreemptAFluxKey(t *testing.T) {
	f := newIngressFixture(t)
	_, key := seedCredential(t, f.store, "flux", "")
	r, ks := uniqueRevision(t), "apps-"+uniqueToken()
	bare := "Kustomization/flux-system/" + ks + "/" + r + "/ReconciliationSucceeded"
	native := post(t, f.srv, key, fmt.Sprintf(`{"type":"t","subject":"s",
		"occurred_at":"2026-10-05T15:00:00Z","idempotency_key":%q}`, bare))
	require.Equal(t, http.StatusCreated, native.Code, native.Body.String())

	rec := postFlux(t, f.srv, key, fluxBody("Kustomization", ks, "ReconciliationSucceeded",
		"info", r, time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, fluxResponse{Recorded: 1, Created: 1}, decodeFlux(t, rec))
}

func changesetBody(ks, revision string, at time.Time, msg string) string {
	return fmt.Sprintf(`{"involvedObject":{"kind":"Kustomization","namespace":"flux-system",
		"name":%q},"severity":"info","timestamp":%q,"message":%q,"reason":"Progressing",
		"metadata":{"revision":%q}}`, ks, at.Format(time.RFC3339), msg, revision)
}

func TestFluxChangesetJoinsTheRolloutWithoutMovingIt(t *testing.T) {
	f := newIngressFixture(t)
	tenant, key := seedCredential(t, f.store, "flux", "")
	r, ks := uniqueRevision(t), "apps-"+uniqueToken()
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	t1, t2 := t0.Add(5*time.Second), t0.Add(9*time.Second)

	require.Equal(t, http.StatusCreated, postFlux(t, f.srv, key, fluxBody("GitRepository",
		"flux-system-"+uniqueToken(), "NewArtifact", "info", r, t0)).Code)
	msg := "Deployment/lastlight/cloudflared configured\nService/lastlight/lastlight created"
	applied := postFlux(t, f.srv, key, changesetBody(ks, r, t1, msg))
	require.Equal(t, http.StatusCreated, applied.Code, applied.Body.String())
	assert.Equal(t, fluxResponse{Recorded: 2, Created: 2}, decodeFlux(t, applied))
	require.Equal(t, http.StatusCreated, postFlux(t, f.srv, key, fluxBody("Kustomization", ks,
		"ReconciliationSucceeded", "info", r, t2)).Code)

	status, start, end := changeRow(t, f.store, tenant, "flux/"+r)
	assert.Equal(t, "completed", status)
	assert.True(t, start.Equal(t0), "NewArtifact still starts it, got %v", start)
	require.NotNil(t, end)
	assert.True(t, end.Equal(t2), "the success still ends it, got %v", *end)

	again := postFlux(t, f.srv, key, changesetBody(ks, r, t1, msg))
	assert.Equal(t, fluxResponse{Recorded: 2, Created: 0}, decodeFlux(t, again),
		"a redelivered changeset is all duplicates")
	assert.Equal(t, 1, countKey(t, f.store,
		"flux/Kustomization/flux-system/"+ks+"/"+r+"/Progressing/Deployment/lastlight/cloudflared"))
}

func TestFluxChangesetOnAnUnseenRevisionIsClosedByItsOutcome(t *testing.T) {
	f := newIngressFixture(t)
	tenant, key := seedCredential(t, f.store, "flux", "")
	r, ks := uniqueRevision(t), "apps-"+uniqueToken()
	t0 := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)

	require.Equal(t, http.StatusCreated, postFlux(t, f.srv, key,
		changesetBody(ks, r, t0, "Deployment/a/b configured")).Code)
	status, _, end := changeRow(t, f.store, tenant, "flux/"+r)
	assert.Equal(t, "in_progress", status, "a none event on a new ref opens it")
	assert.Nil(t, end)

	require.Equal(t, http.StatusCreated, postFlux(t, f.srv, key, fluxBody("Kustomization", ks,
		"ReconciliationSucceeded", "info", r, t0.Add(3*time.Second))).Code)
	status, _, end = changeRow(t, f.store, tenant, "flux/"+r)
	assert.Equal(t, "completed", status, "the same run's outcome closes it")
	assert.NotNil(t, end)
}
