package flux

import (
	"regexp"
	"strconv"
	"time"

	"horopter.dev/horopter/internal/client"
)

// ReleaseMemory is what the client remembers of one HelmRelease between updates: the attempt
// whose start it posted and whose end it has not seen, and the newest revision it closed as
// orphaned. Both are lost on a restart, and a cold list re-derives Open from status.
type ReleaseMemory struct {
	Open     *OpenAttempt
	Orphaned int
}

// OpenAttempt is an install or upgrade whose start was posted: the Helm revision predicted for
// it, its action, and the time its start was posted at, which tells a later start apart.
type OpenAttempt struct {
	Version int
	Action  string
	Since   time.Time
}

// MapHelmRelease brackets each Helm install and upgrade as a release change, ref
// HelmRelease/<ns>/<name>/<uid>/v<revision>. It starts at helm-controller's pre-action patch,
// "Running '<action>' action", and ends when the revision reaches history; a rollback that
// remediates it is a further outcome on the same ref. prev is nil on a cold list, which
// records the newest attempt from history alone, its start approximated as its end less the
// action's duration.
func MapHelmRelease(cluster string, mem ReleaseMemory, prev *HelmRelease, cur HelmRelease,
) ([]client.NewEvent, ReleaseMemory) {
	r := release{cluster: cluster, hr: cur}
	var out []client.NewEvent
	out, mem = r.pending(mem, prev, out)
	out, mem = r.outcomes(mem, prev, out)
	out, mem = r.start(mem, prev, out)
	return out, mem
}

type release struct {
	cluster string
	hr      HelmRelease
}

func (r release) subject() string { return objectPath("HelmRelease", r.hr.Metadata) }

func (r release) ref(version int) string {
	return r.subject() + "/" + r.hr.Metadata.UID + "/v" + strconv.Itoa(version)
}

func (r release) event(version int, typ, lifecycle, keySuffix string, at time.Time,
	attrs map[string]any,
) client.NewEvent {
	ref := r.ref(version)
	return client.NewEvent{
		Type: "flux.helmrelease." + typ, Subject: r.subject(), CorrelationRef: ref,
		IdempotencyKey: r.cluster + "/" + ref + "/" + keySuffix, Lifecycle: lifecycle,
		OccurredAt: at, Attributes: attrs,
	}
}

// pending records helm-controller unlocking a release a dead controller left pending, as a
// point, and closes the attempt that was open, whose revision may never reach history.
func (r release) pending(mem ReleaseMemory, prev *HelmRelease, out []client.NewEvent,
) ([]client.NewEvent, ReleaseMemory) {
	rel, ok := condition(r.hr.Status.Conditions, "Released")
	if !ok || rel.Status != "False" || rel.Reason != "PendingRelease" {
		return out, mem
	}
	if prev != nil {
		if was, ok := condition(prev.Status.Conditions, "Released"); ok && was == rel {
			return out, mem
		}
	}
	if mem.Open != nil {
		out = append(out, r.orphanClosed(*mem.Open, rel.LastTransitionTime))
		mem.Orphaned, mem.Open = max(mem.Orphaned, mem.Open.Version), nil
	}
	attrs := objectAttributes("HelmRelease", r.hr.Metadata)
	attrs["reason"] = rel.Reason
	return append(out, client.NewEvent{
		Type: "flux.helmrelease.pending_release", Subject: r.subject(),
		Title: firstLine(rel.Message), Lifecycle: "none", OccurredAt: rel.LastTransitionTime,
		IdempotencyKey: r.cluster + "/" + r.subject() + "/" + r.hr.Metadata.UID +
			"/PendingRelease/" + rel.LastTransitionTime.UTC().Format(time.RFC3339Nano),
		Attributes: attrs,
	}), mem
}

func (r release) orphanClosed(open OpenAttempt, at time.Time) client.NewEvent {
	ev := r.event(open.Version, open.Action+".failed", "failed", "failed/PendingRelease", at,
		map[string]any{"action": open.Action, "reason": "PendingRelease"})
	ev.Title = r.hr.Status.LastAttemptedRevision
	return ev
}

// running matches helm-controller's Reconciling message immediately before a Helm action.
var running = regexp.MustCompile(`^Running '(install|upgrade)' action`)

// start opens a release change at helm-controller's pre-action patch for an install or
// upgrade. A start while another is open closes that one as orphaned: its controller died.
func (r release) start(mem ReleaseMemory, prev *HelmRelease, out []client.NewEvent,
) ([]client.NewEvent, ReleaseMemory) {
	action, since, ok := r.running()
	if !ok {
		return out, mem
	}
	if mem.Open != nil {
		if mem.Open.Action == action && mem.Open.Since.Equal(since) {
			return out, mem
		}
		out = append(out, r.orphanClosed(*mem.Open, since))
		mem.Orphaned, mem.Open = max(mem.Orphaned, mem.Open.Version), nil
	}
	version := r.predict(action, mem.Orphaned)
	s := r.hr.Status
	attrs := map[string]any{
		"action": action, "chart": r.chart(), "chart_version": s.LastAttemptedRevision,
		"config_digest": s.LastAttemptedConfigDigest, "generation": s.LastAttemptedGeneration,
	}
	if trigger := r.trigger(prev); trigger != "" {
		attrs["trigger"] = trigger
	}
	ev := r.event(version, action+".started", "started", "started", since, attrs)
	ev.Title = s.LastAttemptedRevision
	mem.Open = &OpenAttempt{Version: version, Action: action, Since: since}
	return append(out, ev), mem
}

// running reports the install or upgrade helm-controller is about to run, and when. The
// Reconciling condition keeps its transition time across a retry, so the start is the latest
// of it, Ready's (which every release action turns Unknown), and the previous revision's end.
func (r release) running() (string, time.Time, bool) {
	rec, ok := condition(r.hr.Status.Conditions, "Reconciling")
	if !ok || rec.Status != "True" || rec.Reason != "Progressing" {
		return "", time.Time{}, false
	}
	m := running.FindStringSubmatch(rec.Message)
	if m == nil {
		return "", time.Time{}, false
	}
	since := rec.LastTransitionTime
	if ready, ok := condition(r.hr.Status.Conditions, "Ready"); ok && ready.Reason == "Progressing" {
		since = later(since, ready.LastTransitionTime)
	}
	if h := r.hr.Status.History; len(h) > 0 {
		since = later(since, h[0].LastDeployed)
	}
	return m[1], since, true
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// predict is the Helm revision the action will write: the latest plus one, or 1 for an
// install that does not replace, and never one already closed as orphaned.
func (r release) predict(action string, orphaned int) int {
	latest := 0
	if h := r.hr.Status.History; len(h) > 0 {
		latest = h[0].Version
	}
	next := latest + 1
	if action == "install" && (r.hr.Spec.Install == nil || !r.hr.Spec.Install.Replace) {
		next = 1
	}
	if next <= orphaned {
		next = orphaned + 1
	}
	return next
}

func (r release) chart() string {
	if c := r.hr.Spec.Chart; c != nil && c.Spec.Chart != "" {
		return c.Spec.Chart
	}
	if h := r.hr.Status.History; len(h) > 0 {
		return h[0].ChartName
	}
	return ""
}

// trigger says why helm-controller ran an action: the spec, the chart or the values changed.
// It needs the previous state, so a start seen first on a cold list has none.
func (r release) trigger(prev *HelmRelease) string {
	if prev == nil {
		return ""
	}
	cur, was := r.hr.Status, prev.Status
	switch {
	case cur.LastAttemptedGeneration > was.LastAttemptedGeneration:
		return "spec"
	case cur.LastAttemptedRevision != was.LastAttemptedRevision:
		return "chart"
	case cur.LastAttemptedConfigDigest != was.LastAttemptedConfigDigest:
		return "values"
	}
	return ""
}
