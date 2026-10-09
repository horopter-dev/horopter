// Package flux maps the state of Flux's objects to Horopter events: a GitRepository's new
// artifact starts a commit's change, a Kustomization's Ready condition ends it, and a
// HelmRelease's status brackets each Helm install or upgrade as a release change. Every
// function here is pure: the same states give the same events and idempotency keys, so a
// re-derivation after a restart repeats keys the server already holds.
//
// horopter-internal docs/superpowers/specs/2026-10-07-flux-client-design.md is the design.
package flux

import (
	"time"
)

// Metadata is the part of an object's metadata the mapping reads.
type Metadata struct {
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	UID         string            `json:"uid"`
	Generation  int64             `json:"generation"`
	Annotations map[string]string `json:"annotations"`
}

// Condition is a Kubernetes status condition.
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	Message            string    `json:"message"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// GitRepository is source-controller's source.toolkit.fluxcd.io/v1 GitRepository.
type GitRepository struct {
	Metadata Metadata `json:"metadata"`
	Status   struct {
		Artifact *struct {
			Revision       string    `json:"revision"`
			LastUpdateTime time.Time `json:"lastUpdateTime"`
		} `json:"artifact"`
	} `json:"status"`
}

// Kustomization is kustomize-controller's kustomize.toolkit.fluxcd.io/v1 Kustomization.
type Kustomization struct {
	Metadata Metadata `json:"metadata"`
	Status   struct {
		Conditions            []Condition `json:"conditions"`
		LastAppliedRevision   string      `json:"lastAppliedRevision"`
		LastAttemptedRevision string      `json:"lastAttemptedRevision"`
	} `json:"status"`
}

// HelmRelease is helm-controller's helm.toolkit.fluxcd.io/v2 HelmRelease.
type HelmRelease struct {
	Metadata Metadata `json:"metadata"`
	Spec     struct {
		Chart *struct {
			Spec struct {
				Chart string `json:"chart"`
			} `json:"spec"`
		} `json:"chart"`
		Install *struct {
			Replace bool `json:"replace"`
		} `json:"install"`
	} `json:"spec"`
	Status struct {
		Conditions                         []Condition `json:"conditions"`
		History                            []Snapshot  `json:"history"`
		LastAttemptedGeneration            int64       `json:"lastAttemptedGeneration"`
		LastAttemptedRevision              string      `json:"lastAttemptedRevision"`
		LastAttemptedConfigDigest          string      `json:"lastAttemptedConfigDigest"`
		LastAttemptedReleaseActionDuration string      `json:"lastAttemptedReleaseActionDuration"`
	} `json:"status"`
}

// Snapshot is one entry of a HelmRelease's status.history, newest first. helm-controller
// rewrites a snapshot whenever Helm writes its release again, so Action is reliable only on
// the newest: a superseded snapshot loses it, and a rollback marks the attempt it remediated
// "rollback" while its own new revision carries none.
type Snapshot struct {
	Action       string    `json:"action"`
	Version      int       `json:"version"`
	Status       string    `json:"status"`
	ChartName    string    `json:"chartName"`
	ChartVersion string    `json:"chartVersion"`
	AppVersion   string    `json:"appVersion"`
	ConfigDigest string    `json:"configDigest"`
	LastDeployed time.Time `json:"lastDeployed"`
}

// Event is a core/v1 Kubernetes Event about a Flux object.
type Event struct {
	Metadata       Metadata `json:"metadata"`
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"involvedObject"`
	Reason        string     `json:"reason"`
	Message       string     `json:"message"`
	Count         int        `json:"count"`
	LastTimestamp *time.Time `json:"lastTimestamp"`
	EventTime     *time.Time `json:"eventTime"`
}

func condition(conds []Condition, typ string) (Condition, bool) {
	for _, c := range conds {
		if c.Type == typ {
			return c, true
		}
	}
	return Condition{}, false
}

func objectPath(kind string, m Metadata) string {
	return kind + "/" + m.Namespace + "/" + m.Name
}

func objectAttributes(kind string, m Metadata) map[string]any {
	return map[string]any{"kind": kind, "namespace": m.Namespace, "name": m.Name}
}
