package flux

import (
	"horopter.dev/horopter/internal/client"
)

// MapKustomization records a changed Ready condition as the outcome of the commit it names.
func MapKustomization(cluster string, prev *Kustomization, cur Kustomization,
) []client.NewEvent {
	ready, ok := condition(cur.Status.Conditions, "Ready")
	if !ok {
		return nil
	}
	if unchanged(prev, cur, ready) {
		return nil
	}
	switch {
	case ready.Status == "True" && ready.Reason == "ReconciliationSucceeded":
		return kustomizationOutcome(cluster, cur, ready, cur.Status.LastAppliedRevision,
			"flux.kustomization.succeeded", "completed")
	case ready.Status == "False" && ready.Reason != "DependencyNotReady" &&
		ready.Reason != "Progressing":
		return kustomizationOutcome(cluster, cur, ready, cur.Status.LastAttemptedRevision,
			"flux.kustomization.failed", "failed")
	}
	return nil
}

func unchanged(prev *Kustomization, cur Kustomization, ready Condition) bool {
	if prev == nil {
		return false
	}
	was, ok := condition(prev.Status.Conditions, "Ready")
	return ok && was == ready &&
		prev.Status.LastAppliedRevision == cur.Status.LastAppliedRevision &&
		prev.Status.LastAttemptedRevision == cur.Status.LastAttemptedRevision
}

func kustomizationOutcome(cluster string, ks Kustomization, ready Condition, rev, typ,
	lifecycle string,
) []client.NewEvent {
	if rev == "" {
		return nil
	}
	subject := objectPath("Kustomization", ks.Metadata)
	attrs := objectAttributes("Kustomization", ks.Metadata)
	attrs["reason"], attrs["revision"] = ready.Reason, rev
	ev := client.NewEvent{
		Type: typ, Subject: subject, Title: firstLine(ready.Message),
		CorrelationRef: rev, IdempotencyKey: cluster + "/" + subject + "/" + rev + "/" + ready.Reason,
		Lifecycle: lifecycle, OccurredAt: ready.LastTransitionTime, Attributes: attrs,
	}
	if lifecycle == "completed" {
		if !gitRevision.MatchString(rev) {
			return nil
		}
		return []client.NewEvent{ev}
	}
	point := ev
	point.CorrelationRef, point.IdempotencyKey, point.Lifecycle = "", ev.IdempotencyKey+"/point",
		"none"
	if !gitRevision.MatchString(rev) {
		return []client.NewEvent{point}
	}
	return []client.NewEvent{ev, point}
}
