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
	ev := func(reason, token string, at time.Time) fluxEvent {
		fe := fluxEv("HelmRelease", "podinfo", reason, "info",
			map[string]string{"revision": "6.5.0", "token": token})
		fe.Timestamp = at
		return fe
	}
	got := mapOne(t, ev("UpgradeSucceeded", "d1", fluxAt))
	assert.Equal(t, "flux.helmrelease.upgrade_succeeded", got.Type)
	assert.Equal(t, core.LifecycleNone, got.Lifecycle)
	assert.Empty(t, got.Ref)
	assert.Equal(t,
		"HelmRelease/flux-system/podinfo/6.5.0/d1/UpgradeSucceeded/2026-10-05T14:40:02Z", got.Key)
	assert.Equal(t, "6.5.0", got.Attributes["chart_version"])
	assert.Equal(t, "d1", got.Attributes["values_digest"])
	assert.Equal(t, got.Key, mapOne(t, ev("UpgradeSucceeded", "d1", fluxAt)).Key,
		"a retried delivery resends the same event, so it is a duplicate")
	assert.NotEqual(t, got.Key, mapOne(t, ev("UpgradeSucceeded", "d2", fluxAt)).Key,
		"a values change is recorded")
}

// TestFluxARevertedHelmReleaseIsRecordedAgain pins the timestamp in a success's key: a change
// reverted through Git returns to the same chart and values, and without it the second
// UpgradeSucceeded would repeat the first one's key and never be recorded.
func TestFluxARevertedHelmReleaseIsRecordedAgain(t *testing.T) {
	upgrade := func(at time.Time) fluxEvent {
		fe := fluxEv("HelmRelease", "podinfo", "UpgradeSucceeded", "info",
			map[string]string{"revision": "6.5.0", "token": "d1"})
		fe.Timestamp = at
		return fe
	}
	assert.NotEqual(t, mapOne(t, upgrade(fluxAt)).Key,
		mapOne(t, upgrade(fluxAt.Add(time.Hour))).Key)
}

// TestFluxARepeatedHelmReleaseFailureIsOnePoint keeps a failure's key free of the timestamp:
// helm-controller retries a failing upgrade every interval, and each retry is the same failure.
func TestFluxARepeatedHelmReleaseFailureIsOnePoint(t *testing.T) {
	failed := func(at time.Time) fluxEvent {
		fe := fluxEv("HelmRelease", "podinfo", "UpgradeFailed", "error",
			map[string]string{"revision": "6.5.0", "token": "d1"})
		fe.Timestamp = at
		return fe
	}
	got := mapOne(t, failed(fluxAt))
	assert.Equal(t, "HelmRelease/flux-system/podinfo/6.5.0/d1/UpgradeFailed", got.Key)
	assert.Equal(t, got.Key, mapOne(t, failed(fluxAt.Add(time.Hour))).Key)
}

func TestFluxMetadataBecomesAttributes(t *testing.T) {
	got := mapOne(t, fluxEv("HelmRelease", "podinfo", "UpgradeSucceeded", "info",
		map[string]string{
			"revision": "6.5.0", "token": "d1", "app-version": "v6.5.0", "team": "platform",
		}))
	assert.Equal(t, "v6.5.0", got.Attributes["app-version"],
		"helm-controller's app-version, and any event.toolkit.fluxcd.io/ annotation, is kept")
	assert.Equal(t, "platform", got.Attributes["team"])
}

func TestFluxMetadataKeepsNoneOfFluxsOwnKeys(t *testing.T) {
	got := mapOne(t, fluxEv("HelmRelease", "podinfo", "UpgradeSucceeded", "info",
		map[string]string{
			"revision": "6.5.0", "token": "d1", "originRevision": "o", "commit": "c",
			"commit_status": "update", "change_request": "1", "summary": "s",
		}))
	for _, key := range []string{
		"token", "originRevision", "commit", "commit_status", "change_request", "summary",
	} {
		assert.NotContains(t, got.Attributes, key)
	}
	assert.Equal(t, "6.5.0", got.Attributes["revision"])
}

func TestFluxMetadataCannotOverwriteTheRoutesAttributes(t *testing.T) {
	got := mapOne(t, fluxEv("HelmRelease", "podinfo", "UpgradeSucceeded", "info",
		map[string]string{
			"revision": "6.5.0", "kind": "Spoofed", "severity": "x", "chart_version": "0.0.1",
		}))
	assert.Equal(t, "HelmRelease", got.Attributes["kind"])
	assert.Equal(t, "info", got.Attributes["severity"])
	assert.Equal(t, "6.5.0", got.Attributes["chart_version"])
}

// TestFluxMetadataOverItsBudgetIsDropped keeps an oversized annotation from costing the event:
// the store rejects attributes over its cap, so copied metadata that would push them there is
// left out whole, and the route's own attributes are still recorded.
func TestFluxMetadataOverItsBudgetIsDropped(t *testing.T) {
	got := mapOne(t, fluxEv("HelmRelease", "podinfo", "UpgradeSucceeded", "info",
		map[string]string{
			"revision": "6.5.0", "app-version": "v6.5.0",
			"notes": strings.Repeat("x", maxFluxMetadataBytes),
		}))
	assert.NotContains(t, got.Attributes, "notes")
	assert.NotContains(t, got.Attributes, "app-version")
	assert.Equal(t, "6.5.0", got.Attributes["chart_version"])
}

func TestFluxHelmReleaseWithoutTokenOrRevisionIsStillRecorded(t *testing.T) {
	got := mapOne(t, fluxEv("HelmRelease", "podinfo", "UninstallSucceeded", "info", nil))
	assert.Equal(t, "HelmRelease/flux-system/podinfo///UninstallSucceeded/2026-10-05T14:40:02Z",
		got.Key)
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

func changeset(msg, revision string) fluxEvent {
	fe := fluxEv("Kustomization", "apps", "Progressing", "info", rev(revision))
	fe.Message = msg
	return fe
}

func TestFluxChangesetRecordsOneEventPerObject(t *testing.T) {
	recs, ignored, err := mapFluxEvent(changeset(
		"Deployment/lastlight/cloudflared configured\nNamespace/lastlight created", fluxRev))
	require.NoError(t, err)
	require.Empty(t, ignored)
	require.Len(t, recs, 2)
	head := "Kustomization/flux-system/apps/" + fluxRev + "/Progressing/"
	assert.Equal(t, fluxRecord{
		Type: "flux.kustomization.applied", Subject: "Deployment/lastlight/cloudflared",
		Title: "Deployment/lastlight/cloudflared configured", OccurredAt: fluxAt, Ref: fluxRev,
		Key: head + "Deployment/lastlight/cloudflared", Lifecycle: core.LifecycleNone,
		Attributes: map[string]any{
			"kind": "Kustomization", "namespace": "flux-system", "name": "apps",
			"reason": "Progressing", "severity": "info", "revision": fluxRev,
			"action": "configured",
		},
	}, recs[0])
	assert.Equal(t, "Namespace/lastlight", recs[1].Subject, "a cluster-scoped object is Kind/name")
	assert.Equal(t, head+"Namespace/lastlight", recs[1].Key)
	assert.Equal(t, "created", recs[1].Attributes["action"])
}

func TestFluxChangesetToleratesCRLFAndBlankLines(t *testing.T) {
	recs, ignored, err := mapFluxEvent(changeset(
		"Deployment/a/b configured\r\n\r\nService/a/b created\r\n", fluxRev))
	require.NoError(t, err)
	require.Empty(t, ignored)
	require.Len(t, recs, 2)
	assert.Equal(t, "Deployment/a/b", recs[0].Subject)
	assert.Equal(t, "Service/a/b created", recs[1].Title, "no trailing \\r")
}

func TestFluxChangesetSkipsALineWithNoAction(t *testing.T) {
	recs, _, err := mapFluxEvent(changeset("garbage\nDeployment/a/b configured", fluxRev))
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, "Deployment/a/b", recs[0].Subject)
}

func TestFluxChangesetIgnoredCases(t *testing.T) {
	oci := "latest@sha256:" + strings.Repeat("a", 64)
	for name, tc := range map[string]struct{ msg, rev, why string }{
		"no revision":      {"Deployment/a/b configured", "", "not a Git SHA-1 revision"},
		"a non-Git source": {"Deployment/a/b configured", oci, "not a Git SHA-1 revision"},
		"no object lines":  {"garbage\n\n", fluxRev, "no object lines"},
	} {
		t.Run(name, func(t *testing.T) {
			recs, ignored, err := mapFluxEvent(changeset(tc.msg, tc.rev))
			require.NoError(t, err)
			assert.Empty(t, recs)
			assert.Contains(t, ignored, tc.why)
		})
	}
}

// TestFluxChangesetSkipsLinesThatAreNotObjects drives isObjectPath directly: each line has a
// space, so it reaches the object check, and only the last names an object.
func TestFluxChangesetSkipsLinesThatAreNotObjects(t *testing.T) {
	recs, _, err := mapFluxEvent(changeset(strings.Join([]string{
		"Reconciliation in progress",
		"Deployment configured",
		"a/b/c/d configured",
		"Deployment//x configured",
		"Deployment/a/b configured",
	}, "\n"), fluxRev))
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, "Deployment/a/b", recs[0].Subject)
}

func TestFluxNewArtifactIsTitledByItsCommit(t *testing.T) {
	for _, tc := range []struct{ msg, want string }{
		{"stored artifact for commit 'fix(horopter): roll out v0.5.2'",
			"fix(horopter): roll out v0.5.2"},
		{"stored artifact for commit 'Merge pull request #589 from yo61/fix/horopter-v0....'",
			"Merge pull request #589 from yo61/fix/horopter-v0...."},
		{"stored artifact for commit 'it's quoted'", "it's quoted"},
		{"stored artifact: revision 'refs/heads/main'",
			"stored artifact: revision 'refs/heads/main'"},
	} {
		fe := fluxEv("GitRepository", "flux-system", "NewArtifact", "info", rev(fluxRev))
		fe.Message = tc.msg
		assert.Equal(t, tc.want, mapOne(t, fe).Title, tc.msg)
	}
}
