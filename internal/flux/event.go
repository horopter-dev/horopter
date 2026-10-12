package flux

import (
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"horopter.dev/horopter/internal/client"
)

const (
	kustomizeRevision = "kustomize.toolkit.fluxcd.io/revision"
	helmRevision      = "helm.toolkit.fluxcd.io/revision"
)

// MapEvent records what only a Kubernetes Event carries: a Kustomization's changeset, one
// event per object its apply touched, and HelmRelease outcomes that never reach status.
func MapEvent(cluster string, ev Event) []client.NewEvent {
	switch ev.InvolvedObject.Kind {
	case "Kustomization":
		if ev.Reason == "Progressing" {
			return changeset(cluster, ev)
		}
	case "HelmRelease":
		if eventOnlyOutcome(ev.Reason) {
			return []client.NewEvent{helmReleasePoint(cluster, ev)}
		}
	}
	return nil
}

func (ev Event) object() Metadata {
	o := ev.InvolvedObject
	return Metadata{Namespace: o.Namespace, Name: o.Name}
}

// occurredAt is when the Event last happened: Kubernetes updates one Event in place for a
// repeat, raising count and moving lastTimestamp.
func (ev Event) occurredAt() time.Time {
	if ev.LastTimestamp != nil && !ev.LastTimestamp.IsZero() {
		return *ev.LastTimestamp
	}
	if ev.EventTime != nil {
		return *ev.EventTime
	}
	return time.Time{}
}

// changeset turns each "<object> <action>" line of a Progressing message into an event on the
// revision's change. Lifecycle none: these say what the rollout touched, and move neither its
// start nor its end. Lines that do not name an object are skipped.
func changeset(cluster string, ev Event) []client.NewEvent {
	rev := ev.Metadata.Annotations[kustomizeRevision]
	if !gitRevision.MatchString(rev) {
		return nil
	}
	ks := objectPath("Kustomization", ev.object())
	var out []client.NewEvent
	for line := range strings.SplitSeq(ev.Message, "\n") {
		line = strings.TrimSpace(line)
		cut := strings.LastIndexByte(line, ' ')
		if cut <= 0 || cut == len(line)-1 || !isObjectPath(line[:cut]) {
			continue
		}
		object := line[:cut]
		attrs := objectAttributes("Kustomization", ev.object())
		attrs["reason"], attrs["revision"], attrs["action"] = ev.Reason, rev, line[cut+1:]
		out = append(out, client.NewEvent{
			Type: "flux.kustomization.applied", Subject: object, Title: firstLine(line),
			CorrelationRef: rev,
			IdempotencyKey: cluster + "/" + ks + "/" + rev + "/Progressing/" + object,
			Lifecycle:      "none", OccurredAt: ev.occurredAt(), Attributes: attrs,
		})
	}
	return out
}

// isObjectPath reports whether s is Kind/name or Kind/namespace/name.
func isObjectPath(s string) bool {
	parts := strings.Split(s, "/")
	if strings.ContainsRune(s, ' ') || len(parts) < 2 || len(parts) > 3 {
		return false
	}
	return !slices.Contains(parts, "")
}

// eventOnlyOutcome reports a HelmRelease reason status never shows: drift correction, tests
// and uninstalls.
func eventOnlyOutcome(reason string) bool {
	return reason == "DriftCorrected" || reason == "DriftCorrectionFailed" ||
		strings.HasPrefix(reason, "Test") || strings.HasPrefix(reason, "Uninstall")
}

// helmReleasePoint keys each occurrence of the Event by its uid and count, so a re-delivery
// repeats the key and a repeat of the occurrence does not.
func helmReleasePoint(cluster string, ev Event) client.NewEvent {
	attrs := objectAttributes("HelmRelease", ev.object())
	attrs["reason"], attrs["chart_version"] = ev.Reason, ev.Metadata.Annotations[helmRevision]
	return client.NewEvent{
		Type:           "flux.helmrelease." + snakeCase(ev.Reason),
		Subject:        objectPath("HelmRelease", ev.object()),
		Title:          firstLine(ev.Message),
		IdempotencyKey: cluster + "/Event/" + ev.Metadata.UID + "/" + strconv.Itoa(ev.Count),
		Lifecycle:      "none",
		OccurredAt:     ev.occurredAt(),
		Attributes:     attrs,
	}
}

// snakeCase turns a Flux reason such as DriftCorrected into drift_corrected, keeping an
// acronym one word: OCIArtifactFailed is oci_artifact_failed.
func snakeCase(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 && startsWord(runes, i) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// startsWord reports whether the capital at runes[i] begins a word: it follows a lower-case
// letter, or ends an acronym by preceding one.
func startsWord(runes []rune, i int) bool {
	if !unicode.IsUpper(runes[i-1]) {
		return true
	}
	return i+1 < len(runes) && unicode.IsLower(runes[i+1])
}
