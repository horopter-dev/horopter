package flux

import (
	"regexp"
	"strings"

	"horopter.dev/horopter/internal/client"
)

// gitRevision matches a Git SHA-1 revision; an OCI latest@sha256:… is not known to be unique.
var gitRevision = regexp.MustCompile(`@sha1:[0-9a-f]{40}$`)

// MapGitRepository starts a commit's change when cur's artifact is at a new Git revision.
func MapGitRepository(cluster string, prev *GitRepository, cur GitRepository, title string,
) []client.NewEvent {
	art := cur.Status.Artifact
	if art == nil || !gitRevision.MatchString(art.Revision) {
		return nil
	}
	if prev != nil && prev.Status.Artifact != nil &&
		prev.Status.Artifact.Revision == art.Revision {
		return nil
	}
	subject := objectPath("GitRepository", cur.Metadata)
	attrs := objectAttributes("GitRepository", cur.Metadata)
	attrs["revision"] = art.Revision
	return []client.NewEvent{{
		Type:           "flux.artifact.new",
		Subject:        subject,
		Title:          title,
		CorrelationRef: art.Revision,
		IdempotencyKey: cluster + "/" + subject + "/" + art.Revision + "/NewArtifact",
		Lifecycle:      "started",
		OccurredAt:     art.LastUpdateTime,
		Attributes:     attrs,
	}}
}

// storedArtifact is source-controller's NewArtifact message, quoting the commit's subject.
var storedArtifact = regexp.MustCompile(`^stored artifact for commit '(.*)'$`)

// ArtifactTitle is the commit subject repo's NewArtifact Event quotes, which expires in an hour.
func ArtifactTitle(repo GitRepository, events []Event) (string, bool) {
	if repo.Status.Artifact == nil {
		return "", false
	}
	for _, ev := range events {
		if !announces(ev, repo) {
			continue
		}
		if m := storedArtifact.FindStringSubmatch(firstLine(ev.Message)); m != nil {
			return m[1], true
		}
	}
	return "", false
}

// announces reports whether ev is repo's NewArtifact Event for its current revision.
func announces(ev Event, repo GitRepository) bool {
	o := ev.InvolvedObject
	return o.Kind == "GitRepository" && o.Namespace == repo.Metadata.Namespace &&
		o.Name == repo.Metadata.Name && ev.Reason == "NewArtifact" &&
		ev.Metadata.Annotations[sourceRevision] == repo.Status.Artifact.Revision
}

const sourceRevision = "source.toolkit.fluxcd.io/revision"

// ShortRevision turns refs/heads/main@sha1:b1f2477… into main@b1f2477.
func ShortRevision(rev string) string {
	ref, sha, ok := strings.Cut(rev, "@sha1:")
	if !ok {
		return rev
	}
	ref = strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/")
	return ref + "@" + sha[:min(7, len(sha))]
}

// maxTitle bounds a title taken from a Flux message.
const maxTitle = 200

// firstLine is msg's first line, at most maxTitle characters.
func firstLine(msg string) string {
	first, _, _ := strings.Cut(msg, "\n")
	first = strings.TrimSuffix(first, "\r")
	if r := []rune(first); len(r) > maxTitle {
		return string(r[:maxTitle])
	}
	return first
}
