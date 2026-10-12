package flux_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"horopter.dev/horopter/internal/client"
	"horopter.dev/horopter/internal/flux"
)

const (
	hrUID     = "10aedab5-4046-428a-a051-b87624e0544a"
	hrSubject = "HelmRelease/horopter/horopter"
)

func hrRef(version int) string {
	return fmt.Sprintf("%s/%s/v%d", hrSubject, hrUID, version)
}

// deployed is the captured HelmRelease: v10, an upgrade to chart 0.3.1, deployed.
func deployed(t *testing.T) flux.HelmRelease {
	return load[flux.HelmRelease](t, "helmrelease.yaml")
}

func setCondition(hr *flux.HelmRelease, c flux.Condition) {
	for i := range hr.Status.Conditions {
		if hr.Status.Conditions[i].Type == c.Type {
			hr.Status.Conditions[i] = c
			return
		}
	}
	hr.Status.Conditions = append(hr.Status.Conditions, c)
}

func dropCondition(hr *flux.HelmRelease, typ string) {
	hr.Status.Conditions = slices.DeleteFunc(hr.Status.Conditions,
		func(c flux.Condition) bool { return c.Type == typ })
}

// running is hr after helm-controller's pre-action patch for action, with the chart version
// it is about to roll out.
func running(t *testing.T, hr flux.HelmRelease, action, chartVersion, when string,
) flux.HelmRelease {
	t.Helper()
	hr.Status.Conditions = slices.Clone(hr.Status.Conditions)
	msg := fmt.Sprintf("Running '%s' action with timeout of 10m0s", action)
	setCondition(&hr, flux.Condition{Type: "Reconciling", Status: "True", Reason: "Progressing",
		Message: msg, LastTransitionTime: at(t, when)})
	setCondition(&hr, flux.Condition{Type: "Ready", Status: "Unknown", Reason: "Progressing",
		Message: msg, LastTransitionTime: at(t, when)})
	hr.Status.LastAttemptedRevision = chartVersion
	return hr
}

// finished is hr after a Helm action wrote snap, newest first.
func finished(hr flux.HelmRelease, snap flux.Snapshot, duration string) flux.HelmRelease {
	hr.Status.Conditions = slices.Clone(hr.Status.Conditions)
	dropCondition(&hr, "Reconciling")
	hr.Status.History = append([]flux.Snapshot{snap}, hr.Status.History...)
	hr.Status.LastAttemptedReleaseActionDuration = duration
	return hr
}

// released is hr after helm-controller set Released for the action's outcome, once Helm's wait
// returned: later than the snapshot's lastDeployed, which Helm stamps as it writes the release.
func released(t *testing.T, hr flux.HelmRelease, status, reason, when string) flux.HelmRelease {
	t.Helper()
	hr.Status.Conditions = slices.Clone(hr.Status.Conditions)
	setCondition(&hr, flux.Condition{Type: "Released", Status: status, Reason: reason,
		LastTransitionTime: at(t, when)})
	return hr
}

func snapshot(t *testing.T, action string, version int, status, chartVersion, appVersion,
	lastDeployed string,
) flux.Snapshot {
	return flux.Snapshot{
		Action: action, Version: version, Status: status, ChartName: "horopter",
		ChartVersion: chartVersion, AppVersion: appVersion,
		ConfigDigest: "sha256:89e9590b98d8a88f7d8028d234383da6bb78dd7806d377c531cd2f19d504c35d",
		LastDeployed: at(t, lastDeployed),
	}
}

func keys(events []client.NewEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.IdempotencyKey)
	}
	return out
}

func TestALiveUpgradeIsOneReleaseChangeFromItsStartToHistory(t *testing.T) {
	before := deployed(t)
	during := running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	after := finished(during, snapshot(t, "upgrade", 11, "deployed", "0.3.2", "v0.7.2",
		"2026-10-09T18:00:20Z"), "20s")

	started, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)
	ended, mem := flux.MapHelmRelease(cluster, mem, &during, after)

	assert.Equal(t, []client.NewEvent{{
		Type: "flux.helmrelease.upgrade.started", Subject: hrSubject, Title: "0.3.2",
		CorrelationRef: hrRef(11), IdempotencyKey: cluster + "/" + hrRef(11) + "/started",
		Lifecycle: "started", OccurredAt: at(t, "2026-10-09T18:00:00Z"),
		Attributes: map[string]any{
			"action": "upgrade", "chart": "horopter", "chart_version": "0.3.2",
			"config_digest": before.Status.LastAttemptedConfigDigest, "generation": int64(11),
			"trigger": "chart",
		},
	}}, started)
	assert.Equal(t, []client.NewEvent{{
		Type: "flux.helmrelease.upgrade.succeeded", Subject: hrSubject, Title: "v0.7.2",
		CorrelationRef: hrRef(11), IdempotencyKey: cluster + "/" + hrRef(11) + "/completed",
		Lifecycle: "completed", OccurredAt: at(t, "2026-10-09T18:00:20Z"),
		Attributes: map[string]any{
			"action": "upgrade", "chart": "horopter", "chart_version": "0.3.2",
			"app_version": "v0.7.2", "config_digest": after.Status.History[0].ConfigDigest,
			"helm_revision": 11, "generation": int64(11),
		},
	}}, ended)
	assert.Equal(t, flux.ReleaseMemory{}, mem)
}

func TestALiveUpgradeEndsWhenHelmControllerReleasesIt(t *testing.T) {
	before := deployed(t)
	during := running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	after := released(t, finished(during, snapshot(t, "upgrade", 11, "deployed", "0.3.2",
		"v0.7.2", "2026-10-09T18:00:00Z"), "3m7s"), "True", "UpgradeSucceeded",
		"2026-10-09T18:03:07Z")

	_, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)
	ended, _ := flux.MapHelmRelease(cluster, mem, &during, after)

	require.Len(t, ended, 1)
	assert.Equal(t, at(t, "2026-10-09T18:03:07Z"), ended[0].OccurredAt)
}

func TestAReleasedConditionForAnotherOutcomeLeavesTheEndAtLastDeployed(t *testing.T) {
	before := deployed(t)
	during := running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	after := finished(during, snapshot(t, "upgrade", 11, "deployed", "0.3.2", "v0.7.2",
		"2026-10-09T18:00:20Z"), "20s")

	for name, cur := range map[string]flux.HelmRelease{
		"the previous release's": released(t, after, "True", "UpgradeSucceeded",
			"2026-10-09T17:00:00Z"),
		"another action's": released(t, after, "True", "InstallSucceeded",
			"2026-10-09T18:03:00Z"),
		"another outcome's": released(t, after, "False", "UpgradeFailed",
			"2026-10-09T18:03:00Z"),
	} {
		_, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)
		ended, _ := flux.MapHelmRelease(cluster, mem, &during, cur)

		require.Len(t, ended, 1, name)
		assert.Equal(t, at(t, "2026-10-09T18:00:20Z"), ended[0].OccurredAt, name)
	}
}

func TestTheProgressPatchAfterAnActionStartsNothing(t *testing.T) {
	before := deployed(t)
	during := running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	// helm-controller patches once the action returns, history included, before it clears
	// Reconciling: the Running message is stale.
	progress := finished(during, snapshot(t, "upgrade", 11, "deployed", "0.3.2", "v0.7.2",
		"2026-10-09T18:00:20Z"), "20s")
	progress.Status.Conditions = during.Status.Conditions
	done := finished(during, snapshot(t, "upgrade", 11, "deployed", "0.3.2", "v0.7.2",
		"2026-10-09T18:00:20Z"), "20s")

	_, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)
	ended, mem := flux.MapHelmRelease(cluster, mem, &during, progress)
	settled, mem := flux.MapHelmRelease(cluster, mem, &progress, done)

	assert.Equal(t, []string{cluster + "/" + hrRef(11) + "/completed"}, keys(ended))
	assert.Empty(t, settled)
	assert.Equal(t, flux.ReleaseMemory{}, mem)
}

func TestAFailedUpgradeEndsFailed(t *testing.T) {
	before := deployed(t)
	during := running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	after := finished(during, snapshot(t, "upgrade", 11, "failed", "0.3.2", "v0.7.2",
		"2026-10-09T18:05:00Z"), "5m0s")

	_, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)
	ended, _ := flux.MapHelmRelease(cluster, mem, &during, after)

	require.Len(t, ended, 1)
	assert.Equal(t, "flux.helmrelease.upgrade.failed", ended[0].Type)
	assert.Equal(t, "failed", ended[0].Lifecycle)
	assert.Equal(t, cluster+"/"+hrRef(11)+"/failed", ended[0].IdempotencyKey)
}

func TestAFailedUpgradeEndsWhenHelmControllerMarksItFailed(t *testing.T) {
	before := deployed(t)
	during := running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	after := released(t, finished(during, snapshot(t, "upgrade", 11, "failed", "0.3.2",
		"v0.7.2", "2026-10-09T18:00:00Z"), "5m0s"), "False", "UpgradeFailed",
		"2026-10-09T18:05:00Z")

	_, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)
	ended, _ := flux.MapHelmRelease(cluster, mem, &during, after)

	require.Len(t, ended, 1)
	assert.Equal(t, "flux.helmrelease.upgrade.failed", ended[0].Type)
	assert.Equal(t, at(t, "2026-10-09T18:05:00Z"), ended[0].OccurredAt)
}

func TestAnInstallIsRevisionOne(t *testing.T) {
	fresh := deployed(t)
	fresh.Status.History = nil
	fresh.Status.Conditions = nil
	during := running(t, fresh, "install", "0.3.1", "2026-10-09T18:00:00Z")

	started, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &fresh, during)

	require.Len(t, started, 1)
	assert.Equal(t, "flux.helmrelease.install.started", started[0].Type)
	assert.Equal(t, hrRef(1), started[0].CorrelationRef)
	assert.Equal(t, 1, mem.Open.Version)
}

func TestAnInstallEndsWhenHelmControllerReleasesIt(t *testing.T) {
	fresh := deployed(t)
	fresh.Status.History = nil
	fresh.Status.Conditions = nil
	during := running(t, fresh, "install", "0.3.1", "2026-10-09T18:00:00Z")
	after := released(t, finished(during, snapshot(t, "install", 1, "deployed", "0.3.1",
		"v0.7.0", "2026-10-09T18:00:00Z"), "45s"), "True", "InstallSucceeded",
		"2026-10-09T18:00:45Z")

	_, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &fresh, during)
	ended, _ := flux.MapHelmRelease(cluster, mem, &during, after)

	require.Len(t, ended, 1)
	assert.Equal(t, "flux.helmrelease.install.succeeded", ended[0].Type)
	assert.Equal(t, at(t, "2026-10-09T18:00:45Z"), ended[0].OccurredAt)
}

func TestAnInstallWithReplacePredictsTheNextRevision(t *testing.T) {
	before := deployed(t)
	before.Spec.Install.Replace = true
	during := running(t, before, "install", "0.3.1", "2026-10-09T18:00:00Z")

	started, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)

	assert.Equal(t, hrRef(11), started[0].CorrelationRef)
}

func TestTriggerSaysWhatChangedAndIsAbsentForARetry(t *testing.T) {
	cases := map[string]struct {
		change func(*flux.HelmRelease)
		want   any
	}{
		"spec":  {func(hr *flux.HelmRelease) { hr.Status.LastAttemptedGeneration++ }, "spec"},
		"chart": {func(hr *flux.HelmRelease) { hr.Status.LastAttemptedRevision = "0.4.0" }, "chart"},
		"values": {
			func(hr *flux.HelmRelease) { hr.Status.LastAttemptedConfigDigest = "sha256:new" },
			"values",
		},
		"retry": {func(*flux.HelmRelease) {}, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			before := deployed(t)
			during := running(t, before, "upgrade", before.Status.LastAttemptedRevision,
				"2026-10-09T18:00:00Z")
			c.change(&during)

			started, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)

			require.Len(t, started, 1)
			assert.Equal(t, c.want, started[0].Attributes["trigger"])
		})
	}
}

func TestARetryStartsWhenReadyTurnsUnknownNotWhenReconcilingFirstDid(t *testing.T) {
	failed := finished(running(t, deployed(t), "upgrade", "0.3.2", "2026-10-09T18:00:00Z"),
		snapshot(t, "upgrade", 11, "failed", "0.3.2", "v0.7.2", "2026-10-09T18:05:00Z"), "5m0s")
	// helm-controller left Reconciling True from the first attempt, so its time stands; Ready
	// went False on the failure and turns Unknown again at the retry.
	retry := running(t, failed, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	setCondition(&retry, flux.Condition{Type: "Ready", Status: "Unknown", Reason: "Progressing",
		LastTransitionTime: at(t, "2026-10-09T18:30:00Z")})

	started, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &failed, retry)

	require.Len(t, started, 1)
	assert.Equal(t, hrRef(12), started[0].CorrelationRef)
	assert.Equal(t, at(t, "2026-10-09T18:30:00Z"), started[0].OccurredAt)
}

func TestARestartMidActionRepeatsTheLiveStartAndEnd(t *testing.T) {
	before := deployed(t)
	during := running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	after := finished(during, snapshot(t, "upgrade", 11, "deployed", "0.3.2", "v0.7.2",
		"2026-10-09T18:00:20Z"), "20s")
	liveStart, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)
	liveEnd, _ := flux.MapHelmRelease(cluster, mem, &during, after)

	coldStart, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, nil, during)
	coldEnd, _ := flux.MapHelmRelease(cluster, mem, &during, after)

	// The cold list also re-derives v10, whose events the live client posted before; the
	// server holds their keys already.
	delete(liveStart[0].Attributes, "trigger")
	assert.Equal(t, []string{
		cluster + "/" + hrRef(10) + "/started", cluster + "/" + hrRef(10) + "/completed",
	}, keys(coldStart[:2]))
	assert.Equal(t, liveStart, coldStart[2:])
	assert.Equal(t, liveEnd, coldEnd)
}

func TestAColdListAfterTheActionStartsItAtItsEndLessItsDuration(t *testing.T) {
	before := deployed(t)
	during := running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	after := finished(during, snapshot(t, "upgrade", 11, "deployed", "0.3.2", "v0.7.2",
		"2026-10-09T18:00:20Z"), "20s")
	liveStart, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)
	liveEnd, _ := flux.MapHelmRelease(cluster, mem, &during, after)

	cold, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, nil, after)

	assert.Equal(t, append(keys(liveStart), keys(liveEnd)...), keys(cold))
	assert.Equal(t, at(t, "2026-10-09T18:00:00Z"), cold[0].OccurredAt)
	assert.NotContains(t, cold[0].Attributes, "trigger")
}

func TestAColdListStartsAReleasedAttemptAtReleasedLessItsDuration(t *testing.T) {
	after := released(t, finished(deployed(t), snapshot(t, "upgrade", 11, "deployed", "0.3.2",
		"v0.7.2", "2026-10-09T18:00:00Z"), "3m7s"), "True", "UpgradeSucceeded",
		"2026-10-09T18:03:07Z")

	cold, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, nil, after)

	require.Len(t, cold, 2)
	assert.Equal(t, at(t, "2026-10-09T18:00:00Z"), cold[0].OccurredAt)
	assert.Equal(t, at(t, "2026-10-09T18:03:07Z"), cold[1].OccurredAt)
}

func TestAColdListRecordsOnlyTheNewestAttempt(t *testing.T) {
	first := finished(deployed(t), snapshot(t, "upgrade", 11, "deployed", "0.3.2", "v0.7.2",
		"2026-10-09T18:00:20Z"), "20s")
	first.Status.History[1].Action = ""
	both := finished(first, snapshot(t, "upgrade", 12, "deployed", "0.4.0", "v0.8.0",
		"2026-10-09T19:00:30Z"), "30s")
	both.Status.History[1].Action, both.Status.History[1].Status = "", "superseded"

	cold, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, nil, both)

	assert.Equal(t, []string{
		cluster + "/" + hrRef(12) + "/started", cluster + "/" + hrRef(12) + "/completed",
	}, keys(cold))
}

func TestAnUnchangedHelmReleasePostsNothingAgain(t *testing.T) {
	before := deployed(t)
	labelled := deployed(t)
	labelled.Metadata.Annotations = map[string]string{"team": "platform"}
	intervalEdit := deployed(t)
	intervalEdit.Metadata.Generation++
	intervalEdit.Status.LastAttemptedGeneration++

	for name, cur := range map[string]flux.HelmRelease{
		"a label edit": labelled, "an interval edit": intervalEdit,
	} {
		got, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, cur)
		assert.Empty(t, got, name)
	}
}

// rolledBack is hr after helm-controller rolled a failed v11 upgrade back: v11's snapshot
// rewritten as "rollback" and superseded, and the rollback's own v12 with no action.
func rolledBack(t *testing.T, failed flux.HelmRelease) flux.HelmRelease {
	hr := failed
	hr.Status.History = slices.Clone(failed.Status.History)
	hr.Status.History[0].Action, hr.Status.History[0].Status = "rollback", "superseded"
	hr.Status.History = append([]flux.Snapshot{
		snapshot(t, "", 12, "deployed", "0.3.1", "v0.7.0", "2026-10-09T18:06:00Z"),
	}, hr.Status.History...)
	hr.Status.Conditions = slices.Clone(failed.Status.Conditions)
	setCondition(&hr, flux.Condition{Type: "Remediated", Status: "True",
		Reason: "RollbackSucceeded", LastTransitionTime: at(t, "2026-10-09T18:06:05Z")})
	return hr
}

func TestALiveRollbackEndsTheFailedUpgradeRolledBack(t *testing.T) {
	before := deployed(t)
	during := running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	failed := finished(during, snapshot(t, "upgrade", 11, "failed", "0.3.2", "v0.7.2",
		"2026-10-09T18:05:00Z"), "5m0s")
	after := rolledBack(t, failed)

	_, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)
	_, mem = flux.MapHelmRelease(cluster, mem, &during, failed)
	remediated, _ := flux.MapHelmRelease(cluster, mem, &failed, after)

	require.Len(t, remediated, 1)
	assert.Equal(t, client.NewEvent{
		Type: "flux.helmrelease.upgrade.rolled_back", Subject: hrSubject, Title: "v0.7.2",
		CorrelationRef: hrRef(11),
		IdempotencyKey: cluster + "/" + hrRef(11) + "/rolled_back/RollbackSucceeded",
		Lifecycle:      "rolled_back", OccurredAt: at(t, "2026-10-09T18:06:05Z"),
		Attributes: map[string]any{
			"action": "upgrade", "chart": "horopter", "chart_version": "0.3.2",
			"app_version": "v0.7.2", "config_digest": failed.Status.History[0].ConfigDigest,
			"helm_revision": 11, "generation": int64(11),
		},
	}, remediated[0])
}

func TestARolledBackUpgradeEndsWhereTheLiveWatchSawItFail(t *testing.T) {
	before := deployed(t)
	during := running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	failed := released(t, finished(during, snapshot(t, "upgrade", 11, "failed", "0.3.2",
		"v0.7.2", "2026-10-09T18:00:00Z"), "5m0s"), "False", "UpgradeFailed",
		"2026-10-09T18:05:00Z")
	after := rolledBack(t, failed)

	_, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)
	live, _ := flux.MapHelmRelease(cluster, mem, &during, failed)
	cold, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, nil, after)

	require.Len(t, live, 1)
	require.Len(t, cold, 3)
	assert.Equal(t, at(t, "2026-10-09T18:05:00Z"), live[0].OccurredAt)
	assert.Equal(t, live[0].IdempotencyKey, cold[1].IdempotencyKey)
	assert.Equal(t, at(t, "2026-10-09T18:05:00Z"), cold[1].OccurredAt)
	assert.Equal(t, at(t, "2026-10-09T18:00:00Z"), cold[0].OccurredAt)
}

func TestAColdListOverARolledBackUpgradeIsOneChangeEndingRolledBack(t *testing.T) {
	before := deployed(t)
	failed := finished(running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z"),
		snapshot(t, "upgrade", 11, "failed", "0.3.2", "v0.7.2", "2026-10-09T18:05:00Z"), "5m0s")
	after := rolledBack(t, failed)

	cold, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, nil, after)

	assert.Equal(t, []string{
		cluster + "/" + hrRef(11) + "/started",
		cluster + "/" + hrRef(11) + "/failed",
		cluster + "/" + hrRef(11) + "/rolled_back/RollbackSucceeded",
	}, keys(cold))
	for _, ev := range cold {
		assert.Equal(t, hrRef(11), ev.CorrelationRef)
	}
}

func TestAFailedRollbackEndsTheUpgradeFailed(t *testing.T) {
	before := deployed(t)
	during := running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	failed := finished(during, snapshot(t, "upgrade", 11, "failed", "0.3.2", "v0.7.2",
		"2026-10-09T18:05:00Z"), "5m0s")
	after := failed
	after.Status.Conditions = slices.Clone(failed.Status.Conditions)
	setCondition(&after, flux.Condition{Type: "Remediated", Status: "False",
		Reason: "RollbackFailed", LastTransitionTime: at(t, "2026-10-09T18:06:05Z")})

	got, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &failed, after)

	require.Len(t, got, 1)
	assert.Equal(t, "flux.helmrelease.upgrade.rollback.failed", got[0].Type)
	assert.Equal(t, "failed", got[0].Lifecycle)
	assert.Equal(t, cluster+"/"+hrRef(11)+"/failed/RollbackFailed", got[0].IdempotencyKey)
}

func pendingRelease(t *testing.T, hr flux.HelmRelease, when string) flux.HelmRelease {
	hr.Status.Conditions = slices.Clone(hr.Status.Conditions)
	dropCondition(&hr, "Reconciling")
	setCondition(&hr, flux.Condition{Type: "Released", Status: "False", Reason: "PendingRelease",
		Message: "release horopter/horopter.v11 was unlocked", LastTransitionTime: at(t, when)})
	return hr
}

func TestAnOrphanedStartIsClosedFailedAndTheRetryIsTheNextRevision(t *testing.T) {
	before := deployed(t)
	during := running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	unlocked := pendingRelease(t, during, "2026-10-09T18:10:00Z")
	retry := running(t, unlocked, "upgrade", "0.3.2", "2026-10-09T18:11:00Z")

	_, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)
	closed, mem := flux.MapHelmRelease(cluster, mem, &during, unlocked)
	retried, _ := flux.MapHelmRelease(cluster, mem, &unlocked, retry)

	require.Len(t, closed, 2)
	assert.Equal(t, "flux.helmrelease.upgrade.failed", closed[0].Type)
	assert.Equal(t, hrRef(11), closed[0].CorrelationRef)
	assert.Equal(t, cluster+"/"+hrRef(11)+"/failed/PendingRelease", closed[0].IdempotencyKey)
	assert.Equal(t, "PendingRelease", closed[0].Attributes["reason"])
	assert.Equal(t, "flux.helmrelease.pending_release", closed[1].Type)
	assert.Equal(t, "none", closed[1].Lifecycle)
	require.Len(t, retried, 1)
	assert.Equal(t, hrRef(12), retried[0].CorrelationRef)
}

func TestALaterStartClosesAnOrphanedOne(t *testing.T) {
	before := deployed(t)
	during := running(t, before, "upgrade", "0.3.2", "2026-10-09T18:00:00Z")
	// helm-controller restarted mid-action; the next reconcile ran a new upgrade with no
	// PendingRelease patch in between that the client saw.
	again := running(t, during, "upgrade", "0.3.2", "2026-10-09T18:20:00Z")
	again.Status.LastAttemptedGeneration++

	_, mem := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, &before, during)
	got, _ := flux.MapHelmRelease(cluster, mem, &during, again)

	assert.Equal(t, []string{
		cluster + "/" + hrRef(11) + "/failed/PendingRelease",
		cluster + "/" + hrRef(12) + "/started",
	}, keys(got))
}

func TestARecreatedHelmReleaseRecordsNewChanges(t *testing.T) {
	first := finished(deployed(t), snapshot(t, "install", 1, "deployed", "0.3.1", "v0.7.0",
		"2026-10-09T18:00:20Z"), "20s")
	first.Status.History = first.Status.History[:1]
	second := first
	second.Metadata.UID = "6f0e0b7a-2a39-4c55-9a43-2d3b6c1e9f00"

	a, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, nil, first)
	b, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, nil, second)

	require.Len(t, a, 2)
	require.Len(t, b, 2)
	assert.NotEqual(t, a[0].CorrelationRef, b[0].CorrelationRef)
	for _, k := range keys(b) {
		assert.NotContains(t, keys(a), k)
	}
}

func TestTwoClustersHaveDisjointKeys(t *testing.T) {
	hr := deployed(t)
	other := "0b6f3c0e-7f8f-4b8e-9d55-5a3c8a3f7e21"

	a, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, nil, hr)
	b, _ := flux.MapHelmRelease(other, flux.ReleaseMemory{}, nil, hr)
	ks := load[flux.Kustomization](t, "kustomization.yaml")
	a = append(a, flux.MapKustomization(cluster, nil, ks)...)
	b = append(b, flux.MapKustomization(other, nil, ks)...)

	require.NotEmpty(t, a)
	for _, k := range keys(b) {
		assert.NotContains(t, keys(a), k)
	}
}

func TestTheCapturedHelmReleaseIsAColdListOfItsNewestUpgrade(t *testing.T) {
	cold, _ := flux.MapHelmRelease(cluster, flux.ReleaseMemory{}, nil, deployed(t))

	require.Len(t, cold, 2)
	start := at(t, "2026-10-08T17:02:32Z").Add(-10665418204 * time.Nanosecond)
	assert.Equal(t, start, cold[0].OccurredAt)
	assert.Equal(t, at(t, "2026-10-08T17:02:32Z"), cold[1].OccurredAt)
	assert.Equal(t, "v0.7.0", cold[1].Title)
	assert.Equal(t, hrRef(10), cold[1].CorrelationRef)
}
