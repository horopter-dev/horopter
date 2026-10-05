package ingress

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"horopter.dev/horopter/internal/auth"
	"horopter.dev/horopter/internal/core"
	"horopter.dev/horopter/internal/logging"
)

// fluxEvent is the part of notification-controller's generic-provider payload, Flux's
// eventv1.Event, that the Flux route reads. Other fields are ignored, so a Flux release that
// adds one changes nothing here. notification-controller strips the API-group prefix from
// metadata keys before forwarding, so the revision arrives as plain "revision".
type fluxEvent struct {
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"involvedObject"`
	Severity  string            `json:"severity"`
	Timestamp time.Time         `json:"timestamp"`
	Message   string            `json:"message"`
	Reason    string            `json:"reason"`
	Metadata  map[string]string `json:"metadata"`
}

// fluxRecord is one Horopter event a Flux event maps to, before the credential supplies
// tenant, source and locus. Ref is the producer ref; empty for a point.
type fluxRecord struct {
	Type       string
	Subject    string
	Title      string
	OccurredAt time.Time
	Ref        string
	Key        string
	Lifecycle  core.Lifecycle
	Attributes map[string]any
}

// fluxSeverityError is the severity notification-controller gives a Kubernetes Warning.
const fluxSeverityError = "error"

const maxFluxTitle = 200

// gitRevision matches a Git SHA-1 revision as Flux reports it, e.g.
// refs/heads/main@sha1:<40 hex>. Only these join a change: an OCI source's latest@sha256:…
// needs the mapping record's collision check first.
var gitRevision = regexp.MustCompile(`@sha1:[0-9a-f]{40}$`)

// mapFluxEvent applies decisions/2026-10-04-flux-lifecycle-mapping.md to one Flux event. It
// returns the events to record, or why none are, or an error for an event too incomplete to
// map, which is the caller's 400.
func mapFluxEvent(fe fluxEvent) ([]fluxRecord, string, error) {
	switch {
	case fe.InvolvedObject.Kind == "":
		return nil, "", errors.New("the Flux event has no involvedObject.kind")
	case fe.InvolvedObject.Name == "":
		return nil, "", errors.New("the Flux event has no involvedObject.name")
	case fe.Timestamp.IsZero():
		return nil, "", errors.New("the Flux event has no timestamp")
	}
	switch fe.InvolvedObject.Kind {
	case "GitRepository":
		recs, ignored := mapGitRepository(fe)
		return recs, ignored, nil
	case "Kustomization":
		recs, ignored := mapKustomization(fe)
		return recs, ignored, nil
	case "HelmRelease":
		recs, ignored := mapHelmRelease(fe)
		return recs, ignored, nil
	}
	return nil, fmt.Sprintf("kind %s is not recorded", fe.InvolvedObject.Kind), nil
}

func mapGitRepository(fe fluxEvent) ([]fluxRecord, string) {
	if fe.Reason != "NewArtifact" {
		return nil, fmt.Sprintf("GitRepository reason %s is not recorded", fe.Reason)
	}
	rev := fe.Metadata["revision"]
	if !gitRevision.MatchString(rev) {
		return nil, fmt.Sprintf("revision %q is not a Git SHA-1 revision", rev)
	}
	key := fe.objectPath() + "/" + rev + "/NewArtifact"
	return []fluxRecord{fe.record("flux.artifact.new", core.LifecycleStarted, rev, key)}, ""
}

func mapKustomization(fe fluxEvent) ([]fluxRecord, string) {
	rev := fe.Metadata["revision"]
	key := fe.objectPath() + "/" + rev + "/" + fe.Reason
	const failed = "flux.kustomization.failed"
	switch {
	case fe.Severity == fluxSeverityError && rev == "":
		return nil, "a Kustomization failure with no revision is not recorded"
	case fe.Severity == fluxSeverityError:
		point := fe.record(failed, core.LifecycleNone, "", key+"/point")
		if !gitRevision.MatchString(rev) {
			return []fluxRecord{point}, ""
		}
		return []fluxRecord{fe.record(failed, core.LifecycleFailed, rev, key), point}, ""
	case fe.Reason != "ReconciliationSucceeded":
		return nil, fmt.Sprintf("Kustomization reason %s is not recorded", fe.Reason)
	case !gitRevision.MatchString(rev):
		return nil, fmt.Sprintf("revision %q is not a Git SHA-1 revision", rev)
	}
	return []fluxRecord{
		fe.record("flux.kustomization.succeeded", core.LifecycleCompleted, rev, key),
	}, ""
}

func mapHelmRelease(fe fluxEvent) ([]fluxRecord, string) {
	if !strings.HasSuffix(fe.Reason, "Succeeded") && !strings.HasSuffix(fe.Reason, "Failed") {
		return nil, fmt.Sprintf("HelmRelease reason %s is not recorded", fe.Reason)
	}
	chart, token := fe.Metadata["revision"], fe.Metadata["token"]
	key := fe.objectPath() + "/" + chart + "/" + token + "/" + fe.Reason
	// A success carries its time: a change reverted through Git returns to the same chart and
	// values, and must be recorded again. A failure does not: helm-controller retries a failing
	// upgrade every interval, and each retry is the same failure. Retried deliveries resend the
	// same event, timestamp included, so both stay idempotent.
	if strings.HasSuffix(fe.Reason, "Succeeded") {
		key += "/" + fe.Timestamp.UTC().Format(time.RFC3339Nano)
	}
	rec := fe.record("flux.helmrelease."+snakeCase(fe.Reason), core.LifecycleNone, "", key)
	rec.Attributes["chart_version"] = chart
	rec.Attributes["values_digest"] = token
	return []fluxRecord{rec}, ""
}

// objectPath is <kind>/<namespace>/<name>: the subject, and the head of every key.
func (fe fluxEvent) objectPath() string {
	o := fe.InvolvedObject
	return o.Kind + "/" + o.Namespace + "/" + o.Name
}

func (fe fluxEvent) record(typ string, lc core.Lifecycle, ref, key string) fluxRecord {
	o := fe.InvolvedObject
	return fluxRecord{
		Type: typ, Subject: fe.objectPath(), Title: fluxTitle(fe.Message),
		OccurredAt: fe.Timestamp, Ref: ref, Key: key, Lifecycle: lc,
		Attributes: map[string]any{
			"kind": o.Kind, "namespace": o.Namespace, "name": o.Name,
			"reason": fe.Reason, "severity": fe.Severity, "revision": fe.Metadata["revision"],
		},
	}
}

// fluxTitle is the message's first line, at most maxFluxTitle characters.
func fluxTitle(msg string) string {
	first, _, _ := strings.Cut(msg, "\n")
	if r := []rune(first); len(r) > maxFluxTitle {
		return string(r[:maxFluxTitle])
	}
	return first
}

// snakeCase turns a Flux reason such as UpgradeSucceeded into upgrade_succeeded.
func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// postFluxEvent ingests one event from notification-controller's generic provider and records
// what decisions/2026-10-04-flux-lifecycle-mapping.md maps it to: nothing, one event, or a
// failure's change end and point. An ignored event is answered 200, since a 4xx would make
// notification-controller log a delivery error for every no-op it forwards.
func (s *Server) postFluxEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	var fe fluxEvent
	if err := json.NewDecoder(r.Body).Decode(&fe); err != nil {
		writeBodyError(w, err, "malformed Flux event: "+err.Error()+
			"; expected notification-controller's generic provider payload")
		return
	}
	records, ignored, err := mapFluxEvent(fe)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(records) == 0 {
		logging.Annotate(r.Context(), slog.String("ignored", ignored))
		writeJSON(w, http.StatusOK, map[string]any{"recorded": 0, "ignored": ignored})
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	recordTenant(r.Context(), p.Tenant.String())
	events, err := s.eventsFromFlux(records, p)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if subject, ok := forbiddenSubject(p.SubjectPattern, events); ok {
		subjectForbidden(w, p.SubjectPattern, subject)
		return
	}
	annotateEvent(r.Context(), events[0])
	s.appendFlux(w, r, events)
}

// appendFlux appends events in order and answers for all of them. A failure part-way is a
// 503: every key is idempotent, so notification-controller's retry stores what is missing.
func (s *Server) appendFlux(w http.ResponseWriter, r *http.Request, events []core.Event) {
	created := 0
	for _, ev := range events {
		_, wasCreated, err := s.appendOne(r.Context(), ev)
		if err != nil {
			appendFailed(w, r, ev, err)
			return
		}
		if wasCreated {
			created++
		}
	}
	status := http.StatusOK
	if created > 0 {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"recorded": len(events), "created": created})
}

// forbiddenSubject reports the first event subject the credential's pattern does not cover,
// checked before anything is appended so a refused request stores nothing.
func forbiddenSubject(pattern string, events []core.Event) (string, bool) {
	for _, ev := range events {
		if !subjectAllowed(pattern, ev.Subject) {
			return ev.Subject, true
		}
	}
	return "", false
}

// eventsFromFlux completes each record from the credential. Keys are stored under the
// credential's locus: the store dedupes per tenant, not per locus, so a second cluster's
// identical objects, or a native producer's key of the same shape, would otherwise collide.
func (s *Server) eventsFromFlux(records []fluxRecord, p auth.Principal) ([]core.Event, error) {
	locus := core.LocusFromRow(p.Locus)
	events := make([]core.Event, 0, len(records))
	for _, rec := range records {
		ref, err := resolveRef(locus, rec.Ref, s.allowLocusPrefixedRef)
		if err != nil {
			return nil, err
		}
		now := s.clock()
		events = append(events, core.Event{
			ID:             core.NewULID(now, s.entropy),
			Tenant:         p.Tenant.String(),
			IdempotencyKey: locus.String() + "/" + rec.Key,
			SchemaVersion:  core.CurrentSchemaVersion,
			Source:         p.Source,
			Type:           rec.Type,
			Subject:        rec.Subject,
			Title:          rec.Title,
			OccurredAt:     rec.OccurredAt,
			ReceivedAt:     now,
			CorrelationRef: ref,
			Attributes:     rec.Attributes,
			Origin:         p.Origin,
			Locus:          locus,
			Lifecycle:      rec.Lifecycle,
		})
	}
	return events, nil
}
