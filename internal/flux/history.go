package flux

import (
	"slices"
	"time"

	"horopter.dev/horopter/internal/client"
)

// attempt is the newest install or upgrade in history, and its remediating rollback, if any.
type attempt struct {
	snap        Snapshot
	action      string
	failed      bool
	end         time.Time
	remediation *remediation
}

type remediation struct {
	succeeded bool
	at        time.Time
}

// newestAttempt is history[0], or history[1] when a rollback rewrote it as "rollback".
func newestAttempt(hr HelmRelease) (attempt, bool) {
	h := hr.Status.History
	switch {
	case len(h) > 0 && (h[0].Action == "install" || h[0].Action == "upgrade"):
		return releasedAttempt(hr)
	case len(h) > 1 && h[0].Action == "" && h[1].Action == "rollback":
		return rolledBackAttempt(hr), true
	}
	return attempt{}, false
}

// releasedAttempt is history[0]; a failed rollback leaves no revision of its own.
func releasedAttempt(hr HelmRelease) (attempt, bool) {
	snap := hr.Status.History[0]
	if snap.Status != "deployed" && snap.Status != "failed" {
		return attempt{}, false
	}
	failed := snap.Status == "failed"
	a := attempt{snap: snap, action: snap.Action, failed: failed,
		end: releasedAt(hr, snap, snap.Action, failed)}
	if rem, ok := condition(hr.Status.Conditions, "Remediated"); a.failed && ok &&
		rem.Reason == "RollbackFailed" && rem.LastTransitionTime.After(snap.LastDeployed) {
		a.remediation = &remediation{at: rem.LastTransitionTime}
	}
	return a, true
}

// rolledBackAttempt is the upgrade at history[1] that the rollback at history[0] remediated.
func rolledBackAttempt(hr HelmRelease) attempt {
	h := hr.Status.History
	rem := &remediation{succeeded: h[0].Status == "deployed", at: h[0].LastDeployed}
	if c, ok := condition(hr.Status.Conditions, "Remediated"); ok &&
		(c.Reason == "RollbackSucceeded" || c.Reason == "RollbackFailed") {
		rem = &remediation{succeeded: c.Reason == "RollbackSucceeded", at: c.LastTransitionTime}
	}
	return attempt{snap: h[1], action: "upgrade", failed: true,
		end: releasedAt(hr, h[1], "upgrade", true), remediation: rem}
}

// releasedAt prefers Released: Helm stamps lastDeployed near the action's start, not its end.
func releasedAt(hr HelmRelease, snap Snapshot, action string, failed bool) time.Time {
	rel, ok := condition(hr.Status.Conditions, "Released")
	if ok && rel.Reason == outcomeReason(action, failed) &&
		!rel.LastTransitionTime.Before(snap.LastDeployed) {
		return rel.LastTransitionTime
	}
	return snap.LastDeployed
}

// outcomeReason is the Released reason helm-controller sets for an action's outcome.
func outcomeReason(action string, failed bool) string {
	switch {
	case action == "install" && failed:
		return "InstallFailed"
	case action == "install":
		return "InstallSucceeded"
	case failed:
		return "UpgradeFailed"
	}
	return "UpgradeSucceeded"
}

// outcomes ends the open change's ref, if any, so a mispredicted revision does not leave it open.
func (r release) outcomes(mem ReleaseMemory, prev *HelmRelease, out []client.NewEvent,
) ([]client.NewEvent, ReleaseMemory) {
	a, ok := newestAttempt(r.hr)
	if !ok {
		return out, mem
	}
	version, action := a.snap.Version, a.action
	open := mem.Open != nil && a.snap.Version >= mem.Open.Version
	if open {
		version, action = mem.Open.Version, mem.Open.Action
	}
	seen := r.shownBy(prev)
	fresh := slices.DeleteFunc(r.attemptEvents(a, version, action), func(ev client.NewEvent) bool {
		return slices.Contains(seen, ev.IdempotencyKey)
	})
	switch {
	case len(fresh) == 0:
		return out, mem
	case open:
		mem.Open = nil
	case fresh[0].Type == "flux.helmrelease."+action+".succeeded" ||
		fresh[0].Type == "flux.helmrelease."+action+".failed":
		out = append(out, r.approximateStart(a, version, action))
	}
	return append(out, fresh...), mem
}

// shownBy is the keys of the outcomes prev's history already showed.
func (r release) shownBy(prev *HelmRelease) []string {
	if prev == nil {
		return nil
	}
	was, ok := newestAttempt(*prev)
	if !ok {
		return nil
	}
	return keys(release{r.cluster, *prev}.attemptEvents(was, was.snap.Version, was.action))
}

func keys(events []client.NewEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.IdempotencyKey)
	}
	return out
}

// attemptEvents is the attempt's end, then its remediation's outcome, on revision version.
func (r release) attemptEvents(a attempt, version int, action string) []client.NewEvent {
	attrs := func() map[string]any {
		return map[string]any{
			"action": action, "chart": a.snap.ChartName, "chart_version": a.snap.ChartVersion,
			"app_version": a.snap.AppVersion, "config_digest": a.snap.ConfigDigest,
			"helm_revision": a.snap.Version, "generation": r.hr.Status.LastAttemptedGeneration,
		}
	}
	end := r.event(version, action+".succeeded", "completed", "completed", a.end, attrs())
	if a.failed {
		end = r.event(version, action+".failed", "failed", "failed", a.end, attrs())
	}
	out := []client.NewEvent{end}
	if rem := a.remediation; rem != nil && rem.succeeded {
		out = append(out, r.event(version, action+".rolled_back", "rolled_back",
			"rolled_back/RollbackSucceeded", rem.at, attrs()))
	} else if rem != nil {
		out = append(out, r.event(version, action+".rollback.failed", "failed",
			"failed/RollbackFailed", rem.at, attrs()))
	}
	for i := range out {
		out[i].Title = a.title()
	}
	return out
}

func (a attempt) title() string {
	if a.snap.AppVersion != "" {
		return a.snap.AppVersion
	}
	return a.snap.ChartVersion
}

// approximateStart is an unseen attempt's start: its end less the last action's duration.
func (r release) approximateStart(a attempt, version int, action string) client.NewEvent {
	at := a.end
	if d, err := time.ParseDuration(r.hr.Status.LastAttemptedReleaseActionDuration); err == nil {
		at = at.Add(-d)
	}
	ev := r.event(version, action+".started", "started", "started", at, map[string]any{
		"action": action, "chart": a.snap.ChartName, "chart_version": a.snap.ChartVersion,
		"app_version": a.snap.AppVersion, "config_digest": a.snap.ConfigDigest,
		"generation": r.hr.Status.LastAttemptedGeneration,
	})
	ev.Title = a.title()
	return ev
}
