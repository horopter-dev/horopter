package flux_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"horopter.dev/horopter/internal/client"
	"horopter.dev/horopter/internal/flux"
)

const ksSubject = "Kustomization/flux-system/horopter"

func setReady(ks *flux.Kustomization, status, reason, message, when string) {
	for i, c := range ks.Status.Conditions {
		if c.Type == "Ready" {
			ks.Status.Conditions[i].Status, ks.Status.Conditions[i].Reason = status, reason
			ks.Status.Conditions[i].Message = message
			ts, _ := time.Parse(time.RFC3339, when)
			ks.Status.Conditions[i].LastTransitionTime = ts
		}
	}
}

func TestAKustomizationReadyAtARevisionCompletesItsChange(t *testing.T) {
	cur := load[flux.Kustomization](t, "kustomization.yaml")

	got := flux.MapKustomization(cluster, nil, cur)

	assert.Equal(t, []client.NewEvent{{
		Type:           "flux.kustomization.succeeded",
		Subject:        ksSubject,
		Title:          "Applied revision: " + revision,
		CorrelationRef: revision,
		IdempotencyKey: cluster + "/" + ksSubject + "/" + revision + "/ReconciliationSucceeded",
		Lifecycle:      "completed",
		OccurredAt:     at(t, "2026-10-09T16:30:12Z"),
		Attributes: map[string]any{
			"kind": "Kustomization", "namespace": "flux-system", "name": "horopter",
			"reason": "ReconciliationSucceeded", "revision": revision,
		},
	}}, got)
}

func TestAKustomizationFailureEndsItsChangeAndIsAPoint(t *testing.T) {
	cur := load[flux.Kustomization](t, "kustomization.yaml")
	attempted := "refs/heads/main@sha1:" + strings.Repeat("c", 40)
	cur.Status.LastAttemptedRevision = attempted
	setReady(&cur, "False", "BuildFailed", "kustomize build failed: accumulating resources",
		"2026-10-09T17:00:00Z")

	got := flux.MapKustomization(cluster, nil, cur)

	key := cluster + "/" + ksSubject + "/" + attempted + "/BuildFailed"
	attrs := map[string]any{
		"kind": "Kustomization", "namespace": "flux-system", "name": "horopter",
		"reason": "BuildFailed", "revision": attempted,
	}
	end := client.NewEvent{
		Type: "flux.kustomization.failed", Subject: ksSubject,
		Title:          "kustomize build failed: accumulating resources",
		CorrelationRef: attempted, IdempotencyKey: key, Lifecycle: "failed",
		OccurredAt: at(t, "2026-10-09T17:00:00Z"), Attributes: attrs,
	}
	point := end
	point.CorrelationRef, point.IdempotencyKey, point.Lifecycle = "", key+"/point", "none"
	assert.Equal(t, []client.NewEvent{end, point}, got)
}

func TestAFailureAtANonGitRevisionIsOnlyAPoint(t *testing.T) {
	cur := load[flux.Kustomization](t, "kustomization.yaml")
	cur.Status.LastAttemptedRevision = "latest@sha256:" + strings.Repeat("d", 64)
	setReady(&cur, "False", "HealthCheckFailed", "timeout", "2026-10-09T17:00:00Z")

	got := flux.MapKustomization(cluster, nil, cur)

	if assert.Len(t, got, 1) {
		assert.Equal(t, "none", got[0].Lifecycle)
		assert.Empty(t, got[0].CorrelationRef)
	}
}

func TestWaitingIsNeverAKustomizationOutcome(t *testing.T) {
	for name, set := range map[string][3]string{
		"a dependency":  {"False", "DependencyNotReady", "dependency 'flux-system/db' is not ready"},
		"progressing":   {"False", "Progressing", "reconciliation in progress"},
		"unknown":       {"Unknown", "Progressing", "reconciliation in progress"},
		"no revision":   {"False", "BuildFailed", "x"},
		"ready, no rev": {"True", "ReconciliationSucceeded", "x"},
	} {
		t.Run(name, func(t *testing.T) {
			cur := load[flux.Kustomization](t, "kustomization.yaml")
			setReady(&cur, set[0], set[1], set[2], "2026-10-09T17:00:00Z")
			if name == "no revision" {
				cur.Status.LastAttemptedRevision = ""
			}
			if name == "ready, no rev" {
				cur.Status.LastAppliedRevision = ""
			}

			assert.Empty(t, flux.MapKustomization(cluster, nil, cur))
		})
	}
}

func TestAnUnchangedReadyConditionPostsNothingAgain(t *testing.T) {
	cur := load[flux.Kustomization](t, "kustomization.yaml")
	prev := load[flux.Kustomization](t, "kustomization.yaml")

	assert.Empty(t, flux.MapKustomization(cluster, &prev, cur))
}
