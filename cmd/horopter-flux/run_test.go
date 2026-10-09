package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

// ingest is a stub ingress that records every post by idempotency key and answers with
// respond, 201 by default.
type ingest struct {
	mu      sync.Mutex
	posts   map[string][]map[string]any
	order   []string
	respond func(key string, attempt int) int
}

func newIngest(t *testing.T) (*ingest, string) {
	in := &ingest{posts: map[string][]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		key, _ := body["idempotency_key"].(string)
		in.mu.Lock()
		in.posts[key] = append(in.posts[key], body)
		if len(in.posts[key]) == 1 {
			in.order = append(in.order, key)
		}
		status := http.StatusCreated
		if in.respond != nil {
			status = in.respond(key, len(in.posts[key]))
		}
		in.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "01J", "created": true,
			"error": http.StatusText(status)})
	}))
	t.Cleanup(srv.Close)
	return in, srv.URL
}

func (in *ingest) keys() []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]string(nil), in.order...)
}

func (in *ingest) attempts(key string) int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.posts[key])
}

func (in *ingest) body(key string) map[string]any {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.posts[key][0]
}

func fixture(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	//nolint:gosec // G304: reads the flux package's fixtures by name.
	raw, err := os.ReadFile("../../internal/flux/testdata/" + name)
	require.NoError(t, err)
	js, err := yaml.YAMLToJSON(raw)
	require.NoError(t, err)
	u := &unstructured.Unstructured{}
	require.NoError(t, u.UnmarshalJSON(js))
	return u
}

// runClient runs the client against c and in until the test ends.
func runClient(t *testing.T, c dynamic.Interface, url string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, env(map[string]string{
			"HOROPTER_INGRESS_URL": url, "HOROPTER_INGRESS_KEY": "hpk_test",
		}), c)
	}()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
}

func hrKey(version, suffix string) string {
	return kubeSystemUID + "/HelmRelease/horopter/horopter/10aedab5-4046-428a-a051-b87624e0544a/v" +
		version + "/" + suffix
}

func setStatus(t *testing.T, c dynamic.Interface, hr *unstructured.Unstructured,
	edit func(status map[string]any),
) *unstructured.Unstructured {
	t.Helper()
	hr = hr.DeepCopy()
	status, _, _ := unstructured.NestedMap(hr.Object, "status")
	edit(status)
	require.NoError(t, unstructured.SetNestedMap(hr.Object, status, "status"))
	// The fake tracker leaves resourceVersion as given; an API server bumps it on every write.
	rv, _ := strconv.Atoi(hr.GetResourceVersion())
	hr.SetResourceVersion(strconv.Itoa(rv + 1))
	got, err := c.Resource(helmReleases.gvr).Namespace("horopter").Update(context.Background(), hr,
		metav1.UpdateOptions{})
	require.NoError(t, err)
	return got
}

func TestAnUpgradeIsRecordedFromItsStatusAsItHappens(t *testing.T) {
	in, url := newIngest(t)
	hr := fixture(t, "helmrelease.yaml")
	c := fakeCluster(kubeSystem(), hr)
	runClient(t, c, url)

	require.Eventually(t, func() bool { return in.attempts(hr10Completed) == 1 },
		5*time.Second, 10*time.Millisecond)
	during := setStatus(t, c, hr, func(s map[string]any) {
		msg := "Running 'upgrade' action with timeout of 10m0s"
		s["conditions"] = []any{
			map[string]any{"type": "Reconciling", "status": "True", "reason": "Progressing",
				"message": msg, "lastTransitionTime": "2026-10-09T18:00:00Z"},
			map[string]any{"type": "Ready", "status": "Unknown", "reason": "Progressing",
				"message": msg, "lastTransitionTime": "2026-10-09T18:00:00Z"},
		}
		s["lastAttemptedRevision"] = "0.3.2"
	})
	require.Eventually(t, func() bool { return in.attempts(hrKey("11", "started")) == 1 },
		5*time.Second, 10*time.Millisecond)
	setStatus(t, c, during, func(s map[string]any) {
		s["conditions"] = []any{map[string]any{"type": "Ready", "status": "True",
			"reason": "UpgradeSucceeded", "lastTransitionTime": "2026-10-09T18:00:21Z"}}
		s["history"] = append([]any{map[string]any{
			"action": "upgrade", "version": int64(11), "status": "deployed", "chartName": "horopter",
			"chartVersion": "0.3.2", "appVersion": "v0.7.2", "lastDeployed": "2026-10-09T18:00:20Z",
		}}, s["history"].([]any)...)
	})
	require.Eventually(t, func() bool { return in.attempts(hrKey("11", "completed")) == 1 },
		5*time.Second, 10*time.Millisecond)

	assert.Equal(t, []string{
		hrKey("10", "started"), hr10Completed, hrKey("11", "started"), hrKey("11", "completed"),
	}, in.keys())
	assert.Equal(t, "chart", in.body(hrKey("11", "started"))["attributes"].(map[string]any)["trigger"])
	assert.Equal(t, "2026-10-09T18:00:20Z", in.body(hrKey("11", "completed"))["occurred_at"])
}

var hr10Completed = hrKey("10", "completed")

func TestA503IsRetriedUntilTheEventIsRecorded(t *testing.T) {
	in, url := newIngest(t)
	in.respond = func(_ string, attempt int) int {
		if attempt == 1 {
			return http.StatusServiceUnavailable
		}
		return http.StatusCreated
	}
	runClient(t, fakeCluster(kubeSystem(), fixture(t, "helmrelease.yaml")), url)

	require.Eventually(t, func() bool { return in.attempts(hr10Completed) == 2 },
		5*time.Second, 10*time.Millisecond)
}

// captureLogs swaps the process-wide slog default for one writing JSON to a buffer. Setting
// slog's default also redirects the stdlib log package, so that is restored as well.
func captureLogs(t *testing.T) func() []map[string]any {
	var mu sync.Mutex
	var buf bytes.Buffer
	prev, writer, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewJSONHandler(syncWriter{&mu, &buf}, nil)))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		var out []map[string]any
		for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) == nil {
				out = append(out, rec)
			}
		}
		return out
	}
}

type syncWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func TestARefusedEventIsLoggedOnceAndNotRetried(t *testing.T) {
	retryBase = time.Millisecond
	t.Cleanup(func() { retryBase = time.Second })
	logs := captureLogs(t)
	in, url := newIngest(t)
	in.respond = func(string, int) int { return http.StatusUnauthorized }
	runClient(t, fakeCluster(kubeSystem(), fixture(t, "helmrelease.yaml")), url)

	require.Eventually(t, func() bool { return in.attempts(hr10Completed) == 1 },
		5*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)

	assert.Equal(t, 1, in.attempts(hr10Completed))
	var refusals int
	for _, rec := range logs() {
		if rec["level"] == "ERROR" && rec["key"] == hr10Completed {
			refusals++
			assert.Equal(t,
				"HelmRelease/horopter/horopter/10aedab5-4046-428a-a051-b87624e0544a/v10", rec["ref"])
		}
	}
	assert.Equal(t, 1, refusals)
}

func gitEvent(t *testing.T, subject string) *unstructured.Unstructured {
	ev := &unstructured.Unstructured{}
	ev.SetAPIVersion("v1")
	ev.SetKind("Event")
	ev.SetNamespace("flux-system")
	ev.SetName("flux-system.newartifact")
	ev.SetAnnotations(map[string]string{"source.toolkit.fluxcd.io/revision": gitRevision})
	require.NoError(t, unstructured.SetNestedMap(ev.Object, map[string]any{
		"kind": "GitRepository", "namespace": "flux-system", "name": "flux-system",
	}, "involvedObject"))
	ev.Object["reason"] = "NewArtifact"
	ev.Object["message"] = "stored artifact for commit '" + subject + "'"
	return ev
}

const gitRevision = "refs/heads/main@sha1:b1f2477dc2c215154e22f674405c3fde594fd494"

var gitKey = kubeSystemUID + "/GitRepository/flux-system/flux-system/" + gitRevision +
	"/NewArtifact"

// freshRepo is the captured GitRepository with its artifact stored just now.
func freshRepo(t *testing.T) *unstructured.Unstructured {
	repo := fixture(t, "gitrepository.yaml")
	require.NoError(t, unstructured.SetNestedField(repo.Object,
		time.Now().UTC().Format(time.RFC3339), "status", "artifact", "lastUpdateTime"))
	return repo
}

func TestAFreshCommitWaitsForItsNewArtifactEventToBeTitled(t *testing.T) {
	titleRetry = 20 * time.Millisecond
	t.Cleanup(func() { titleRetry = 5 * time.Second })
	in, url := newIngest(t)
	c := fakeCluster(kubeSystem(), freshRepo(t), fixture(t, "helmrelease.yaml"))
	runClient(t, c, url)

	// The HelmRelease's posts show the client has listed everything and is posting.
	require.Eventually(t, func() bool { return in.attempts(hr10Completed) == 1 },
		5*time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, in.attempts(gitKey), "posted before its title arrived")
	_, err := c.Resource(events).Namespace("flux-system").Create(context.Background(),
		gitEvent(t, "feat: record Flux"), metav1.CreateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return in.attempts(gitKey) == 1 },
		5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "feat: record Flux", in.body(gitKey)["title"])
}

func TestACommitWhoseEventNeverArrivesIsTitledByItsRevision(t *testing.T) {
	titleWait, titleRetry = 50*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { titleWait, titleRetry = time.Minute, 5*time.Second })
	in, url := newIngest(t)
	runClient(t, fakeCluster(kubeSystem(), freshRepo(t)), url)

	require.Eventually(t, func() bool { return in.attempts(gitKey) == 1 },
		5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "main@b1f2477", in.body(gitKey)["title"])
}

func TestTransientServerErrorsAreRetriedUntilRecorded(t *testing.T) {
	retryBase = time.Millisecond
	t.Cleanup(func() { retryBase = time.Second })
	for _, status := range []int{
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			in, url := newIngest(t)
			in.respond = func(_ string, attempt int) int {
				if attempt == 1 {
					return status
				}
				return http.StatusCreated
			}
			runClient(t, fakeCluster(kubeSystem(), fixture(t, "helmrelease.yaml")), url)

			require.Eventually(t, func() bool { return in.attempts(hr10Completed) == 2 },
				5*time.Second, 10*time.Millisecond)
		})
	}
}

func driftEvent(count int64) *unstructured.Unstructured {
	ev := &unstructured.Unstructured{}
	ev.SetAPIVersion("v1")
	ev.SetKind("Event")
	ev.SetNamespace("horopter")
	ev.SetName("horopter.drift")
	ev.SetUID("4b1c0b52-0d2e-4f63-9d1e-0f4c2f6f1a11")
	ev.SetResourceVersion(strconv.FormatInt(count, 10))
	_ = unstructured.SetNestedMap(ev.Object, map[string]any{
		"kind": "HelmRelease", "namespace": "horopter", "name": "horopter",
	}, "involvedObject")
	ev.Object["reason"] = "DriftCorrected"
	ev.Object["message"] = "drift corrected"
	ev.Object["count"] = int64(1)
	ev.Object["lastTimestamp"] = "2026-10-09T18:00:00Z"
	return ev
}

func TestAnEventDerivedAgainWhilePostingIsNotPostedEmpty(t *testing.T) {
	in, url := newIngest(t)
	release := make(chan struct{})
	driftKey := kubeSystemUID + "/Event/4b1c0b52-0d2e-4f63-9d1e-0f4c2f6f1a11/1"
	in.respond = func(key string, _ int) int {
		if key == driftKey {
			in.mu.Unlock()
			<-release
			in.mu.Lock()
		}
		return http.StatusCreated
	}
	c := fakeCluster(kubeSystem(), driftEvent(1))
	runClient(t, c, url)
	require.Eventually(t, func() bool { return in.attempts(driftKey) == 1 },
		5*time.Second, 10*time.Millisecond)

	// Re-delivered at the same count while its first post is in flight.
	_, err := c.Resource(events).Namespace("horopter").Update(context.Background(),
		driftEvent(2), metav1.UpdateOptions{})
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)
	close(release)
	time.Sleep(200 * time.Millisecond)

	assert.Zero(t, in.attempts(""), "posted an event with no idempotency key")
}
