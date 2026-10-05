package ingress

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"horopter.dev/horopter/internal/core"
)

const fluxRev = "refs/heads/main@sha1:9115ab8df1a7c89d04bd37e8271efdd93aa371f8"

var fluxAt = time.Date(2026, 10, 5, 14, 40, 2, 0, time.UTC)

// fluxEv builds a Flux event about object kind flux-system/<name>.
func fluxEv(kind, name, reason, severity string, meta map[string]string) fluxEvent {
	fe := fluxEvent{
		Severity: severity, Timestamp: fluxAt, Reason: reason, Metadata: meta,
		Message: "Reconciliation finished in 1.2s\nnext run in 1h",
	}
	fe.InvolvedObject.Kind = kind
	fe.InvolvedObject.Namespace = "flux-system"
	fe.InvolvedObject.Name = name
	return fe
}

func rev(r string) map[string]string { return map[string]string{"revision": r} }

func mapOne(t *testing.T, fe fluxEvent) fluxRecord {
	t.Helper()
	recs, ignored, err := mapFluxEvent(fe)
	require.NoError(t, err)
	require.Empty(t, ignored)
	require.Len(t, recs, 1)
	return recs[0]
}

func TestFluxNewArtifactStartsAChange(t *testing.T) {
	got := mapOne(t, fluxEv("GitRepository", "flux-system", "NewArtifact", "info", rev(fluxRev)))
	assert.Equal(t, fluxRecord{
		Type: "flux.artifact.new", Subject: "GitRepository/flux-system/flux-system",
		Title: "Reconciliation finished in 1.2s", OccurredAt: fluxAt, Ref: fluxRev,
		Key:       "GitRepository/flux-system/flux-system/" + fluxRev + "/NewArtifact",
		Lifecycle: core.LifecycleStarted,
		Attributes: map[string]any{
			"kind": "GitRepository", "namespace": "flux-system", "name": "flux-system",
			"reason": "NewArtifact", "severity": "info", "revision": fluxRev,
		},
	}, got)
}

func TestFluxSuccessEndsTheChange(t *testing.T) {
	got := mapOne(t, fluxEv("Kustomization", "apps", "ReconciliationSucceeded", "info",
		rev(fluxRev)))
	assert.Equal(t, "flux.kustomization.succeeded", got.Type)
	assert.Equal(t, core.LifecycleCompleted, got.Lifecycle)
	assert.Equal(t, fluxRev, got.Ref)
	assert.Equal(t, "Kustomization/flux-system/apps/"+fluxRev+"/ReconciliationSucceeded", got.Key)
	assert.Equal(t, "Kustomization/flux-system/apps", got.Subject)
}

func TestFluxFailureEndsTheChangeAndRecordsAPoint(t *testing.T) {
	recs, ignored, err := mapFluxEvent(
		fluxEv("Kustomization", "apps", "HealthCheckFailed", "error", rev(fluxRev)))
	require.NoError(t, err)
	require.Empty(t, ignored)
	require.Len(t, recs, 2)
	end, point := recs[0], recs[1]
	key := "Kustomization/flux-system/apps/" + fluxRev + "/HealthCheckFailed"
	assert.Equal(t, core.LifecycleFailed, end.Lifecycle)
	assert.Equal(t, fluxRev, end.Ref)
	assert.Equal(t, key, end.Key)
	assert.Equal(t, core.LifecycleNone, point.Lifecycle)
	assert.Empty(t, point.Ref, "a point carries no ref")
	assert.Equal(t, key+"/point", point.Key,
		"sharing the change end's key would make one of the two a duplicate")
	assert.Equal(t, "flux.kustomization.failed", end.Type)
	assert.Equal(t, "flux.kustomization.failed", point.Type)
}

func TestFluxFailureAtANonGitRevisionRecordsOnlyThePoint(t *testing.T) {
	oci := "latest@sha256:" + strings.Repeat("a", 64)
	got := mapOne(t, fluxEv("Kustomization", "apps", "HealthCheckFailed", "error", rev(oci)))
	assert.Equal(t, core.LifecycleNone, got.Lifecycle)
	assert.Empty(t, got.Ref)
	assert.True(t, strings.HasSuffix(got.Key, "/point"))
}

func TestFluxIgnoresARevisionlessFailure(t *testing.T) {
	recs, ignored, err := mapFluxEvent(
		fluxEv("Kustomization", "apps", "ArtifactFailed", "error", nil))
	require.NoError(t, err)
	assert.Empty(t, recs, "with no revision, the point's key would collapse every such failure")
	assert.Contains(t, ignored, "no revision")
}

func TestFluxIgnores(t *testing.T) {
	cases := map[string]fluxEvent{
		"a dependency wait": fluxEv("Kustomization", "apps", "DependencyNotReady", "info",
			rev(fluxRev)),
		"a Kustomization info event other than success": fluxEv("Kustomization", "apps",
			"Progressing", "info", rev(fluxRev)),
		"a GitRepository reason other than NewArtifact": fluxEv("GitRepository", "flux-system",
			"GitOperationFailed", "error", rev(fluxRev)),
		"an OCI artifact": fluxEv("GitRepository", "flux-system", "NewArtifact", "info",
			rev("latest@sha256:"+strings.Repeat("a", 64))),
		"a success at an OCI revision": fluxEv("Kustomization", "apps",
			"ReconciliationSucceeded", "info", rev("latest@sha256:"+strings.Repeat("a", 64))),
		"uppercase hex": fluxEv("Kustomization", "apps", "ReconciliationSucceeded", "info",
			rev("refs/heads/main@sha1:"+strings.Repeat("A", 40))),
		"a short hash": fluxEv("Kustomization", "apps", "ReconciliationSucceeded", "info",
			rev("refs/heads/main@sha1:9115ab8")),
		"a SHA-256 Git revision": fluxEv("Kustomization", "apps", "ReconciliationSucceeded",
			"info", rev("refs/heads/main@sha256:"+strings.Repeat("a", 64))),
		"a HelmRelease reason that is no outcome": fluxEv("HelmRelease", "podinfo",
			"DependencyNotReady", "info", nil),
		"an OCIRepository": fluxEv("OCIRepository", "app", "NewArtifact", "info",
			rev(fluxRev)),
		"a HelmChart": fluxEv("HelmChart", "app", "ChartPullSucceeded", "info", nil),
	}
	for name, fe := range cases {
		t.Run(name, func(t *testing.T) {
			recs, ignored, err := mapFluxEvent(fe)
			require.NoError(t, err)
			assert.Empty(t, recs)
			assert.NotEmpty(t, ignored, "an ignored event must say why")
		})
	}
}

func TestFluxHelmReleaseIsAPointKeyedByChartAndValues(t *testing.T) {
	ev := func(token string) fluxEvent {
		return fluxEv("HelmRelease", "podinfo", "UpgradeSucceeded", "info",
			map[string]string{"revision": "6.5.0", "token": token})
	}
	got := mapOne(t, ev("d1"))
	assert.Equal(t, "flux.helmrelease.upgrade_succeeded", got.Type)
	assert.Equal(t, core.LifecycleNone, got.Lifecycle)
	assert.Empty(t, got.Ref)
	assert.Equal(t, "HelmRelease/flux-system/podinfo/6.5.0/d1/UpgradeSucceeded", got.Key)
	assert.Equal(t, "6.5.0", got.Attributes["chart_version"])
	assert.Equal(t, "d1", got.Attributes["values_digest"])
	assert.Equal(t, got.Key, mapOne(t, ev("d1")).Key, "a repeat is a duplicate")
	assert.NotEqual(t, got.Key, mapOne(t, ev("d2")).Key, "a values change is recorded")
}

func TestFluxHelmReleaseWithoutTokenOrRevisionIsStillRecorded(t *testing.T) {
	got := mapOne(t, fluxEv("HelmRelease", "podinfo", "UninstallSucceeded", "info", nil))
	assert.Equal(t, "HelmRelease/flux-system/podinfo///UninstallSucceeded", got.Key)
	assert.Equal(t, "flux.helmrelease.uninstall_succeeded", got.Type)
}

func TestFluxTitleIsTheFirstLineCappedAt200Characters(t *testing.T) {
	fe := fluxEv("Kustomization", "apps", "ReconciliationSucceeded", "info", rev(fluxRev))
	fe.Message = strings.Repeat("é", 250) + "\nrest"
	assert.Equal(t, strings.Repeat("é", 200), mapOne(t, fe).Title)
	fe.Message = ""
	assert.Empty(t, mapOne(t, fe).Title, "an empty message still records the event")
}

func TestFluxRequiresKindNameAndTimestamp(t *testing.T) {
	for name, mutate := range map[string]func(*fluxEvent){
		"kind":      func(fe *fluxEvent) { fe.InvolvedObject.Kind = "" },
		"name":      func(fe *fluxEvent) { fe.InvolvedObject.Name = "" },
		"timestamp": func(fe *fluxEvent) { fe.Timestamp = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			fe := fluxEv("Kustomization", "apps", "ReconciliationSucceeded", "info", rev(fluxRev))
			mutate(&fe)
			_, _, err := mapFluxEvent(fe)
			require.Error(t, err)
			assert.Contains(t, err.Error(), name)
		})
	}
}

// TestFluxDecodesNotificationControllersPayload decodes the payload as notification-controller
// v1.9.2 sends it, fields the route never reads included, plus one a later Flux might add.
func TestFluxDecodesNotificationControllersPayload(t *testing.T) {
	body := `{"involvedObject":{"kind":"Kustomization","namespace":"flux-system","name":"apps",
		"uid":"0b6c","apiVersion":"kustomize.toolkit.fluxcd.io/v1"},
		"severity":"info","timestamp":"2026-10-05T14:40:02Z",
		"message":"Reconciliation finished","reason":"ReconciliationSucceeded",
		"metadata":{"revision":"` + fluxRev + `"},
		"reportingController":"kustomize-controller","reportingInstance":"kc-0",
		"aFieldFromALaterFlux":{"x":1}}`
	var fe fluxEvent
	require.NoError(t, json.Unmarshal([]byte(body), &fe))
	got := mapOne(t, fe)
	assert.Equal(t, core.LifecycleCompleted, got.Lifecycle)
	assert.Equal(t, fluxAt, got.OccurredAt)
}

func TestSnakeCase(t *testing.T) {
	for in, want := range map[string]string{
		"UpgradeSucceeded": "upgrade_succeeded", "InstallFailed": "install_failed",
		"TestSucceeded": "test_succeeded", "x": "x",
	} {
		assert.Equal(t, want, snakeCase(in), in)
	}
}
