package flux_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"horopter.dev/horopter/internal/client"
	"horopter.dev/horopter/internal/flux"
)

const (
	cluster    = "a853062b-a841-4c7a-b3f6-946cc27abb46"
	revision   = "refs/heads/main@sha1:b1f2477dc2c215154e22f674405c3fde594fd494"
	gitSubject = "GitRepository/flux-system/flux-system"
)

func load[T any](t *testing.T, name string) T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: fixtures.
	require.NoError(t, err)
	var v T
	require.NoError(t, yaml.Unmarshal(raw, &v))
	return v
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return ts
}

func TestANewGitArtifactStartsTheCommitsChange(t *testing.T) {
	cur := load[flux.GitRepository](t, "gitrepository.yaml")

	got := flux.MapGitRepository(cluster, nil, cur, "Add the flux client")

	assert.Equal(t, []client.NewEvent{{
		Type:           "flux.artifact.new",
		Subject:        gitSubject,
		Title:          "Add the flux client",
		CorrelationRef: revision,
		IdempotencyKey: cluster + "/" + gitSubject + "/" + revision + "/NewArtifact",
		Lifecycle:      "started",
		OccurredAt:     at(t, "2026-10-09T15:26:44Z"),
		Attributes: map[string]any{
			"kind": "GitRepository", "namespace": "flux-system", "name": "flux-system",
			"revision": revision,
		},
	}}, got)
}

func TestAnArtifactAlreadySeenStartsNothing(t *testing.T) {
	cur := load[flux.GitRepository](t, "gitrepository.yaml")
	prev := cur

	assert.Empty(t, flux.MapGitRepository(cluster, &prev, cur, "Add the flux client"))
}

func TestAnOCIArtifactStartsNothing(t *testing.T) {
	cur := load[flux.GitRepository](t, "gitrepository.yaml")
	cur.Status.Artifact.Revision = "latest@sha256:" + strings.Repeat("a", 64)

	assert.Empty(t, flux.MapGitRepository(cluster, nil, cur, "x"))
}

func newArtifactEvent(rev, message string) flux.Event {
	var ev flux.Event
	ev.InvolvedObject.Kind, ev.InvolvedObject.Namespace = "GitRepository", "flux-system"
	ev.InvolvedObject.Name = "flux-system"
	ev.Reason, ev.Message = "NewArtifact", message
	ev.Metadata.Annotations = map[string]string{"source.toolkit.fluxcd.io/revision": rev}
	return ev
}

func TestTheCommitTitleIsTheSubjectItsNewArtifactEventQuotes(t *testing.T) {
	repo := load[flux.GitRepository](t, "gitrepository.yaml")
	events := []flux.Event{
		newArtifactEvent("refs/heads/main@sha1:"+strings.Repeat("0", 40),
			"stored artifact for commit 'Older'"),
		newArtifactEvent(revision, "stored artifact for commit 'feat: add the flux client'"),
	}

	title, ok := flux.ArtifactTitle(repo, events)

	assert.True(t, ok)
	assert.Equal(t, "feat: add the flux client", title)
}

func TestANewArtifactEventEndingInACarriageReturnStillTitlesItsCommit(t *testing.T) {
	repo := load[flux.GitRepository](t, "gitrepository.yaml")
	ev := newArtifactEvent(revision, "stored artifact for commit 'feat: add the flux client'\r\n")

	title, ok := flux.ArtifactTitle(repo, []flux.Event{ev})

	assert.True(t, ok)
	assert.Equal(t, "feat: add the flux client", title)
}

func TestWithoutItsNewArtifactEventACommitHasNoTitleYet(t *testing.T) {
	repo := load[flux.GitRepository](t, "gitrepository.yaml")
	otherName := newArtifactEvent(revision, "stored artifact for commit 'Elsewhere'")
	otherName.InvolvedObject.Name = "another-repo"
	otherNamespace := newArtifactEvent(revision, "stored artifact for commit 'Elsewhere'")
	otherNamespace.InvolvedObject.Namespace = "team-a"

	_, ok := flux.ArtifactTitle(repo, []flux.Event{otherName, otherNamespace})

	assert.False(t, ok)
}

func TestTheFallbackTitleIsTheBranchAndShortSHA(t *testing.T) {
	assert.Equal(t, "main@b1f2477", flux.ShortRevision(revision))
	assert.Equal(t, "v1.2.0@b1f2477",
		flux.ShortRevision("refs/tags/v1.2.0@sha1:b1f2477dc2c215154e22f674405c3fde594fd494"))
}
