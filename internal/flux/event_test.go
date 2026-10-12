package flux_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"horopter.dev/horopter/internal/client"
	"horopter.dev/horopter/internal/flux"
)

func changesetEvent(t *testing.T) flux.Event {
	ev := load[flux.Event](t, "event-kustomization-succeeded.yaml")
	ev.Reason = "Progressing"
	ev.Message = "HelmRelease/horopter/horopter configured\n" +
		"Service/horopter/horopter-ingress unchanged\n" +
		"this line is prose, not an object\n" +
		"Namespace/horopter created"
	return ev
}

func TestEachChangesetLineIsAnEventOnTheCommitsChange(t *testing.T) {
	ev := changesetEvent(t)

	got := flux.MapEvent(cluster, ev)

	ts := at(t, "2026-10-09T16:30:12Z")
	line := func(object, action string) client.NewEvent {
		return client.NewEvent{
			Type: "flux.kustomization.applied", Subject: object, Title: object + " " + action,
			CorrelationRef: revision,
			IdempotencyKey: cluster + "/" + ksSubject + "/" + revision + "/Progressing/" + object,
			Lifecycle:      "none", OccurredAt: ts,
			Attributes: map[string]any{
				"kind": "Kustomization", "namespace": "flux-system", "name": "horopter",
				"reason": "Progressing", "revision": revision, "action": action,
			},
		}
	}
	assert.Equal(t, []client.NewEvent{
		line("HelmRelease/horopter/horopter", "configured"),
		line("Service/horopter/horopter-ingress", "unchanged"),
		line("Namespace/horopter", "created"),
	}, got)
}

func TestAChangesetAtANonGitRevisionRecordsNothing(t *testing.T) {
	ev := changesetEvent(t)
	ev.Metadata.Annotations["kustomize.toolkit.fluxcd.io/revision"] = "latest@sha256:abc"

	assert.Empty(t, flux.MapEvent(cluster, ev))
}

func TestOtherKustomizationEventsRecordNothing(t *testing.T) {
	ev := load[flux.Event](t, "event-kustomization-succeeded.yaml")

	assert.Empty(t, flux.MapEvent(cluster, ev))
}

func helmReleaseEvent(reason string, count int, last time.Time) flux.Event {
	var ev flux.Event
	ev.Metadata.UID = "4b1c0b52-0d2e-4f63-9d1e-0f4c2f6f1a11"
	ev.Metadata.Annotations = map[string]string{"helm.toolkit.fluxcd.io/revision": "0.3.1"}
	ev.InvolvedObject.Kind, ev.InvolvedObject.Namespace = "HelmRelease", "horopter"
	ev.InvolvedObject.Name = "horopter"
	ev.Reason, ev.Message, ev.Count, ev.LastTimestamp = reason, "drift corrected", count, &last
	return ev
}

func TestEventOnlyHelmReleaseOutcomesArePoints(t *testing.T) {
	last := at(t, "2026-10-09T18:00:00Z")
	for _, reason := range []string{
		"DriftCorrected", "DriftCorrectionFailed", "TestSucceeded", "TestFailed",
		"UninstallSucceeded", "UninstallFailed",
	} {
		t.Run(reason, func(t *testing.T) {
			got := flux.MapEvent(cluster, helmReleaseEvent(reason, 1, last))

			if assert.Len(t, got, 1) {
				assert.Equal(t, "none", got[0].Lifecycle)
				assert.Empty(t, got[0].CorrelationRef)
				assert.Equal(t, "HelmRelease/horopter/horopter", got[0].Subject)
				assert.Equal(t, last, got[0].OccurredAt)
				assert.Equal(t, "0.3.1", got[0].Attributes["chart_version"])
			}
		})
	}
	snake := flux.MapEvent(cluster, helmReleaseEvent("DriftCorrectionFailed", 1, last))
	assert.Equal(t, "flux.helmrelease.drift_correction_failed", snake[0].Type)
}

func TestAnAcronymInAReasonStaysOneWord(t *testing.T) {
	last := at(t, "2026-10-09T18:00:00Z")
	for reason, want := range map[string]string{
		"UninstallOCIArtifactFailed": "flux.helmrelease.uninstall_oci_artifact_failed",
		"TestHTTPTimeout":            "flux.helmrelease.test_http_timeout",
		"TestFailedOCI":              "flux.helmrelease.test_failed_oci",
	} {
		t.Run(reason, func(t *testing.T) {
			got := flux.MapEvent(cluster, helmReleaseEvent(reason, 1, last))

			if assert.Len(t, got, 1) {
				assert.Equal(t, want, got[0].Type)
			}
		})
	}
}

func TestACarriageReturnLineEndingIsNotPartOfTheTitle(t *testing.T) {
	ev := helmReleaseEvent("DriftCorrected", 1, at(t, "2026-10-09T18:00:00Z"))
	ev.Message = "drift corrected\r\nDeployment/horopter/horopter"

	got := flux.MapEvent(cluster, ev)

	if assert.Len(t, got, 1) {
		assert.Equal(t, "drift corrected", got[0].Title)
	}
}

func TestStatusVisibleHelmReleaseEventsAreNotPoints(t *testing.T) {
	last := at(t, "2026-10-09T18:00:00Z")
	for _, reason := range []string{"UpgradeSucceeded", "InstallFailed", "RollbackSucceeded"} {
		assert.Empty(t, flux.MapEvent(cluster, helmReleaseEvent(reason, 1, last)), reason)
	}
}

func TestARepeatedEventIsOnePointPerOccurrence(t *testing.T) {
	drift := func(count int, last string) []client.NewEvent {
		return flux.MapEvent(cluster, helmReleaseEvent("DriftCorrected", count, at(t, last)))
	}
	first := drift(3, "2026-10-09T18:00:00Z")
	again := drift(3, "2026-10-09T18:00:00Z")
	next := drift(4, "2026-10-09T18:05:00Z")

	assert.Equal(t, first[0].IdempotencyKey, again[0].IdempotencyKey)
	assert.Equal(t, cluster+"/Event/4b1c0b52-0d2e-4f63-9d1e-0f4c2f6f1a11/3", first[0].IdempotencyKey)
	assert.NotEqual(t, first[0].IdempotencyKey, next[0].IdempotencyKey)
}

func TestAnEventWithoutLastTimestampUsesEventTime(t *testing.T) {
	ev := helmReleaseEvent("DriftCorrected", 1, time.Time{})
	ev.LastTimestamp = nil
	when := at(t, "2026-10-09T18:10:00Z")
	ev.EventTime = &when

	assert.Equal(t, when, flux.MapEvent(cluster, ev)[0].OccurredAt)
}
