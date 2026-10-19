package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func shortWindows(t *testing.T) {
	t.Helper()
	s, e, a := startWindow, endWindow, abortWindow
	short := 300 * time.Millisecond
	startWindow, endWindow, abortWindow = short, short, short
	t.Cleanup(func() { startWindow, endWindow, abortWindow = s, e, a })
}

func runCmd(t *testing.T, url string, args ...string) (int, string, string) {
	t.Helper()
	return record(t, url, append([]string{"run", "--type", "deployment", "--subject", "acme/prod"},
		args...)...)
}

func TestRunRecordsACompletedCommand(t *testing.T) {
	f := &fakeIngress{}
	code, out, errOut := runCmd(t, f.server(t), "--", "sh", "-c", "echo hello")
	require.Equal(t, 0, code)
	assert.Equal(t, "hello\n", out, "stdout is the child's alone")
	posts := f.posted()
	require.Len(t, posts, 2)
	assert.Equal(t, "deployment.started", posts[0]["type"])
	assert.Equal(t, "started", posts[0]["lifecycle"])
	assert.Equal(t, "deployment.completed", posts[1]["type"])
	assert.Equal(t, "completed", posts[1]["lifecycle"])
	assert.Equal(t, posts[0]["correlation_ref"], posts[1]["correlation_ref"])
	start, _ := posts[0]["idempotency_key"].(string)
	end, _ := posts[1]["idempotency_key"].(string)
	assert.True(t, strings.HasSuffix(start, "/start") && strings.HasSuffix(end, "/end"))
	assert.Equal(t, strings.TrimSuffix(start, "/start"), strings.TrimSuffix(end, "/end"),
		"one invocation, one id")
	ref, _ := posts[0]["correlation_ref"].(string)
	assert.Contains(t, errOut, "ci/"+ref+" (horo show)")
	assert.Contains(t, errOut, "ref "+ref+" (--ref)")
}

func TestRunRecordsAFailureAndKeepsItsOutputAndCode(t *testing.T) {
	f := &fakeIngress{}
	code, out, _ := runCmd(t, f.server(t), "--", "sh", "-c", "echo partial; exit 3")
	assert.Equal(t, 3, code)
	assert.Equal(t, "partial\n", out)
	end := f.posted()[1]
	assert.Equal(t, "deployment.failed", end["type"])
	assert.Equal(t, map[string]any{"exit_code": float64(3)}, end["attributes"])
}

func TestRunRecordsACommandKilledByASignal(t *testing.T) {
	f := &fakeIngress{}
	code, _, _ := runCmd(t, f.server(t), "--", "sh", "-c", "kill -TERM $$")
	assert.Equal(t, 128+int(syscall.SIGTERM), code)
	end := f.posted()[1]
	assert.Equal(t, "deployment.cancelled", end["type"])
	assert.Equal(t, "cancelled", end["lifecycle"])
	assert.Equal(t, map[string]any{"signal": "SIGTERM"}, end["attributes"])
}

func TestRunRecordsACommandThatCannotStart(t *testing.T) {
	f := &fakeIngress{}
	code, _, _ := runCmd(t, f.server(t), "--", "/nonexistent/horo-test-binary")
	assert.Equal(t, 127, code)
	end := f.posted()[1]
	assert.Equal(t, "deployment.failed", end["type"])
	assert.Contains(t, end["attributes"], "error")
}

func TestTwoRunsUseDifferentInvocationIDs(t *testing.T) {
	f := &fakeIngress{}
	url := f.server(t)
	runCmd(t, url, "--ref", "same", "--", "true")
	runCmd(t, url, "--ref", "same", "--", "true")
	posts := f.posted()
	assert.NotEqual(t, posts[0]["idempotency_key"], posts[2]["idempotency_key"])
}

func TestRunStillRunsTheCommandWhenHoropterIsDown(t *testing.T) {
	shortWindows(t)
	code, out, errOut := runCmd(t, "http://127.0.0.1:1", "--", "sh", "-c", "echo ran; exit 4")
	assert.Equal(t, 4, code)
	assert.Equal(t, "ran\n", out)
	assert.Contains(t, errOut, "could not record the start")
}

func TestNoEndIsPostedWhenTheStartWasNotRecorded(t *testing.T) {
	shortWindows(t)
	f := &fakeIngress{status: http.StatusServiceUnavailable}
	code, _, errOut := runCmd(t, f.server(t), "--", "sh", "-c", "exit 6")
	assert.Equal(t, 6, code)
	for _, body := range f.posted() {
		assert.Equal(t, "started", body["lifecycle"], "only start attempts, never an end")
	}
	assert.Contains(t, errOut, "outcome is not recorded")
}

func TestStrictDoesNotRunTheCommandWhenTheStartFails(t *testing.T) {
	shortWindows(t)
	marker := filepath.Join(t.TempDir(), "ran")
	code, _, errOut := runCmd(t, "http://127.0.0.1:1", "--strict", "--", "touch", marker)
	assert.Equal(t, 1, code)
	_, err := os.Stat(marker)
	assert.True(t, os.IsNotExist(err), "the command must not have run")
	assert.Contains(t, errOut, "the command was not run", "a CI log must say the deploy did not run")
}

// A start that timed out or met a 5xx may have been stored anyway.
func TestStrictClosesAStartThatMayHaveBeenStored(t *testing.T) {
	shortWindows(t)
	runRefused := func(t *testing.T, f *fakeIngress) (int, string, map[string]any) {
		t.Helper()
		marker := filepath.Join(t.TempDir(), "ran")
		path := reportFile(t)
		code, _, errOut := runCmd(t, startFailingIngress{}.server(t, f.server(t)),
			"--strict", "--ref", "dep-1", "--report", path, "--", "touch", marker)
		assert.NoFileExists(t, marker, "the command must not have run")
		return code, errOut, readReport(t, path)
	}
	t.Run("cancellation confirmed", func(t *testing.T) {
		f := &fakeIngress{}
		code, errOut, got := runRefused(t, f)
		assert.Equal(t, 1, code)
		posts := f.posted()
		require.Len(t, posts, 1)
		assert.Equal(t, "deployment.cancelled", posts[0]["type"])
		assert.Equal(t, "cancelled", posts[0]["lifecycle"])
		assert.Equal(t, "dep-1", posts[0]["correlation_ref"])
		key, _ := posts[0]["idempotency_key"].(string)
		assert.True(t, strings.HasSuffix(key, "/end"), "the end's key: a late close replays it")
		assert.Contains(t, errOut, "the command was not run, and is recorded as cancelled")
		assert.Equal(t, true, got["end_posted"])
		assert.Equal(t, "ci/dep-1", got["ref"])
		assert.Equal(t, "cancelled", got["outcome"])
		assert.Equal(t, float64(1), got["exit_code"])
		assert.Contains(t, got["reason"], "could not record the start")
	})
	t.Run("cancellation refused", func(t *testing.T) {
		code, errOut, got := runRefused(t, &fakeIngress{status: http.StatusInternalServerError})
		assert.Equal(t, 1, code)
		args := shellWords(t, recoveryLine(t, errOut))
		assert.Subset(t, args, []string{"--ref", "dep-1", "--lifecycle", "cancelled"})
		assert.Equal(t, false, got["end_posted"])
		assert.Equal(t, "cancelled", got["outcome"])
		assert.Contains(t, got["reason"], "could not record the start")
		assert.Contains(t, got["reason"], "the cancellation could not be confirmed")
	})
	// A 429 is a hop in front of ingress: it says nothing of an earlier attempt that timed out.
	for _, status := range []int{http.StatusInternalServerError, http.StatusTooManyRequests} {
		t.Run(fmt.Sprintf("start answered %d", status), func(t *testing.T) {
			f := &fakeIngress{}
			url := startFailingIngress{status: status}.server(t, f.server(t))
			code, _, _ := runCmd(t, url, "--strict", "--", "true")
			assert.Equal(t, 1, code)
			posts := f.posted()
			require.Len(t, posts, 1)
			assert.Equal(t, "cancelled", posts[0]["lifecycle"])
		})
	}
}

// A refusal is an answer: the start was not stored.
func TestStrictLeavesNothingToCloseWhenIngressRefusedTheStart(t *testing.T) {
	shortWindows(t)
	f := &fakeIngress{status: http.StatusUnauthorized}
	code, _, errOut := runCmd(t, f.server(t), "--strict", "--", "true")
	assert.Equal(t, 1, code)
	posts := f.posted()
	require.Len(t, posts, 1, "the start alone, never a cancellation")
	assert.Equal(t, "started", posts[0]["lifecycle"])
	assert.Contains(t, errOut, "--strict, so the command was not run")
	assert.NotContains(t, errOut, "horo event ")
}

// An unset CI secret is Horopter being unreachable, not a reason to fail the deploy.
func TestAMissingKeyStillRunsTheCommand(t *testing.T) {
	env := func(k string) string {
		return map[string]string{"HOROPTER_INGRESS_URL": "http://127.0.0.1:1"}[k]
	}
	marker := filepath.Join(t.TempDir(), "ran")
	var out, errOut bytes.Buffer
	code := run([]string{"run", "--type", "deployment", "--subject", "s", "--",
		"sh", "-c", `touch "$1"; exit 5`, "sh", marker}, env, &out, &errOut, false)
	assert.Equal(t, 5, code)
	assert.FileExists(t, marker)
	assert.Contains(t, errOut.String(), "HOROPTER_INGRESS_KEY")

	strictMarker := filepath.Join(t.TempDir(), "ran")
	code = run([]string{"run", "--strict", "--type", "deployment", "--subject", "s", "--",
		"touch", strictMarker}, env, &out, &errOut, false)
	assert.Equal(t, 1, code, "the same code as a refusal after a failed start")
	assert.NoFileExists(t, strictMarker)
}

// The printed command, run as horo event against the same fake, must reach the same change.
func TestAFailedEndPrintsACommandThatClosesTheChange(t *testing.T) {
	shortWindows(t)
	f := &fakeIngress{}
	url := f.server(t)
	failEnd := &endFailingIngress{inner: f}
	code, _, errOut := record(t, failEnd.server(t, url), "run", "--type", "deployment",
		"--subject", "acme prod's api", "--", "true")
	assert.Equal(t, 0, code, "not --strict: the command's own code")
	line := recoveryLine(t, errOut)
	args := shellWords(t, line)
	require.Equal(t, []string{"horo", "event"}, args[:2])
	ended := time.Now()
	time.Sleep(20 * time.Millisecond)
	code, _, _ = record(t, url, args[1:]...)
	require.Equal(t, 0, code)
	posts := f.posted()
	start, closing := posts[0], posts[len(posts)-1]
	closedAt, err := time.Parse(time.RFC3339Nano, closing["occurred_at"].(string))
	require.NoError(t, err)
	assert.True(t, closedAt.Before(ended), "the close carries when the command ended, not now")
	assert.Equal(t, start["correlation_ref"], closing["correlation_ref"])
	startKey, _ := start["idempotency_key"].(string)
	assert.Equal(t, strings.TrimSuffix(startKey, "/start")+"/end", closing["idempotency_key"],
		"the close reuses the end post's key, so it cannot add a second end")
	assert.Equal(t, "acme prod's api", closing["subject"])
	assert.Equal(t, "deployment.completed", closing["type"])
}

// Without --, a flag after the command would silently reach the command.
func TestRunNeedsACommandAfterTheDashes(t *testing.T) {
	for _, args := range [][]string{{"--"}, {}, {"./deploy.sh"}, {"./deploy.sh", "--strict"}} {
		code, _, errOut := runCmd(t, (&fakeIngress{}).server(t), args...)
		assert.Equal(t, 2, code)
		assert.Contains(t, errOut, "--")
	}
	code, _, errOut := record(t, (&fakeIngress{}).server(t), "run", "./deploy.sh")
	assert.Equal(t, 2, code, "a command as the very first argument: no flag precedes it")
	assert.Contains(t, errOut, "--")
}

// The SIGINT case also proves the end post survives run's context being cancelled.
func TestRunForwardsSignalsToTheChild(t *testing.T) {
	saved := sharesTerminal
	sharesTerminal = func() bool { return false }
	t.Cleanup(func() { sharesTerminal = saved })
	for _, sig := range []syscall.Signal{
		syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT,
	} {
		t.Run(sig.String(), func(t *testing.T) {
			f := &fakeIngress{}
			ready := filepath.Join(t.TempDir(), "ready")
			name := map[syscall.Signal]string{
				syscall.SIGTERM: "TERM", syscall.SIGINT: "INT",
				syscall.SIGHUP: "HUP", syscall.SIGQUIT: "QUIT",
			}[sig]
			script := `trap 'exit 0' ` + name + `; touch "$1"; while :; do sleep 0.05; done`
			done := make(chan int, 1)
			go func() {
				code, _, _ := runCmd(t, f.server(t), "--", "sh", "-c", script, "sh", ready)
				done <- code
			}()
			require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil },
				5*time.Second, 20*time.Millisecond)
			require.NoError(t, syscall.Kill(os.Getpid(), sig))
			select {
			case code := <-done:
				assert.Equal(t, 0, code)
			case <-time.After(5 * time.Second):
				t.Fatal("horo run did not return after the signal")
			}
			posts := f.posted()
			require.Len(t, posts, 2, "the end post must survive the interrupt")
			assert.Equal(t, "deployment.completed", posts[1]["type"])
		})
	}
}

// An interrupt during the start post aborts, and horo must survive it to close the change.
func TestCtrlCDuringTheStartDoesNotRunTheCommand(t *testing.T) {
	shortWindows(t)
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			types := make(chan string, 4)
			release := make(chan struct{})
			hang := func(_ http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				typ, _ := body["type"].(string)
				types <- typ
				<-release
			}
			srv := httptest.NewServer(http.HandlerFunc(hang))
			t.Cleanup(func() { close(release); srv.Close() })
			marker := filepath.Join(t.TempDir(), "ran")
			done := make(chan int, 1)
			go func() {
				code, _, _ := runCmd(t, srv.URL, "--", "touch", marker)
				done <- code
			}()
			require.Equal(t, "deployment.started", <-types)
			require.NoError(t, syscall.Kill(os.Getpid(), sig))
			select {
			case code := <-done:
				assert.Equal(t, 128+int(sig), code)
			case <-time.After(5 * time.Second):
				t.Fatal("horo run did not return after the signal during the start")
			}
			_, err := os.Stat(marker)
			assert.True(t, os.IsNotExist(err), "the command must not have run")
			select {
			case typ := <-types:
				assert.Equal(t, "deployment.cancelled", typ, "the start is closed")
			default:
				t.Fatal("no cancellation was posted")
			}
		})
	}
}

// Against a silent Horopter, the second Ctrl-C ends the cancellation post at once.
func TestASecondInterruptSkipsTheCancellationPost(t *testing.T) {
	s, a := startWindow, abortWindow
	startWindow, abortWindow = 300*time.Millisecond, 30*time.Second
	t.Cleanup(func() { startWindow, abortWindow = s, a })
	types := make(chan string, 4)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		typ, _ := body["type"].(string)
		types <- typ
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	done := make(chan int, 1)
	var errOut string
	go func() {
		code, _, e := runCmd(t, srv.URL, "--", "true")
		errOut = e
		done <- code
	}()
	require.Equal(t, "deployment.started", <-types)
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
	require.Equal(t, "deployment.cancelled", <-types)
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
	select {
	case code := <-done:
		assert.Equal(t, 128+int(syscall.SIGINT), code)
		assert.Contains(t, errOut, "horo event ", "the close command is still printed")
	case <-time.After(2 * time.Second):
		t.Fatal("a second interrupt must end horo at once")
	}
}

// An interrupt stops the end post's wait rather than being swallowed for endWindow.
func TestAnInterruptDuringTheEndPostPrintsTheCloseCommand(t *testing.T) {
	e := endWindow
	endWindow = 30 * time.Second
	t.Cleanup(func() { endWindow = e })
	ends := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["lifecycle"] == "started" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ref":"ci/r","created":true}`))
			return
		}
		ends <- struct{}{}
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	path := reportFile(t)
	done := make(chan int, 1)
	var errOut string
	go func() {
		code, _, e := runCmd(t, srv.URL, "--report", path, "--", "sh", "-c", "exit 4")
		errOut = e
		done <- code
	}()
	<-ends
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
	select {
	case code := <-done:
		assert.Equal(t, 4, code, "the command's own code")
		assert.Contains(t, errOut, "horo event ")
	case <-time.After(2 * time.Second):
		t.Fatal("an interrupt during the end post must end the wait")
	}
	got := readReport(t, path)
	assert.Equal(t, true, got["start_posted"])
	assert.Equal(t, false, got["end_posted"])
	assert.Equal(t, "ci/r", got["ref"])
	assert.Equal(t, "failed", got["outcome"])
	assert.Contains(t, got["reason"], "interrupted while waiting for Horopter")
}

// The end's longer window must not apply before the command starts.
func TestASilentHoropterDelaysTheCommandByTheStartWindowOnly(t *testing.T) {
	s, e := startWindow, endWindow
	startWindow, endWindow = 300*time.Millisecond, 3*time.Second
	t.Cleanup(func() { startWindow, endWindow = s, e })
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	marker := filepath.Join(t.TempDir(), "ran")
	begin := time.Now()
	go func() { _, _, _ = runCmd(t, srv.URL, "--", "touch", marker) }()
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil },
		2*time.Second, 20*time.Millisecond, "the command must start once the start window ends")
	assert.Less(t, time.Since(begin), 2*time.Second)
}

// /dev/null is a character device, which a mode check mistakes for a terminal.
func TestDevNullIsNotATerminal(t *testing.T) {
	assert.False(t, inForeground(os.DevNull))
	assert.False(t, inForeground(filepath.Join(t.TempDir(), "missing")))
}

func TestSIGINTIsNotForwardedFromATerminal(t *testing.T) {
	assert.Equal(t, []os.Signal{syscall.SIGTERM}, forwarded(true))
	assert.ElementsMatch(t,
		[]os.Signal{syscall.SIGTERM, os.Interrupt, syscall.SIGHUP, syscall.SIGQUIT}, forwarded(false))
}

// endFailingIngress forwards to inner but answers every end post (an /end key) with 400.
type endFailingIngress struct{ inner *fakeIngress }

func (e *endFailingIngress) server(t *testing.T, innerURL string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `/end"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"end refused"}`))
			return
		}
		forward(t, w, innerURL+r.URL.Path, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// startFailingIngress answers a start with status, or never when 0, and forwards other posts.
type startFailingIngress struct{ status int }

func (s startFailingIngress) server(t *testing.T, innerURL string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `/start"`) {
			if s.status == 0 {
				<-r.Context().Done()
				return
			}
			w.WriteHeader(s.status)
			return
		}
		forward(t, w, innerURL+r.URL.Path, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// forward posts body to url and copies the answer to w.
func forward(t *testing.T, w http.ResponseWriter, url string, body []byte) {
	t.Helper()
	//nolint:gosec // G704: forwards to the test's own httptest server.
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// recoveryLine finds the "horo event ..." command in a failed-end warning.
func recoveryLine(t *testing.T, stderr string) string {
	t.Helper()
	for _, l := range strings.Split(stderr, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "horo event ") {
			return l
		}
	}
	t.Fatalf("no recovery command in %q", stderr)
	return ""
}

// shellWords splits a line the way a POSIX shell splits quoted words.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	var w words
	for _, r := range line {
		w.add(r)
	}
	require.False(t, w.quoted || w.dquoted, "unbalanced quote in %q", line)
	w.end()
	return w.list
}

// words is shellWords' state: the words so far, the one being built, and which quotes are open.
type words struct {
	list                    []string
	cur                     strings.Builder
	inWord, quoted, dquoted bool
}

func (w *words) add(r rune) {
	switch {
	case r == '\'' && !w.dquoted:
		w.quoted, w.inWord = !w.quoted, true
	case r == '"' && !w.quoted:
		w.dquoted, w.inWord = !w.dquoted, true
	case r == ' ' && !w.quoted && !w.dquoted:
		w.end()
	default:
		w.cur.WriteRune(r)
		w.inWord = true
	}
}

func (w *words) end() {
	if w.inWord {
		w.list = append(w.list, w.cur.String())
		w.cur.Reset()
		w.inWord = false
	}
}

// A tab, or the trailing newline a CI variable often carries, must still paste as one word.
func TestShellQuoteSurvivesARealShell(t *testing.T) {
	for _, s := range []string{
		"plain", "", "acme prod", "it's", "a\tb", "trailing\n", "=cmd", "$HOME", "*", `a"b`,
		`back\slash`, "ci/dep-1:2026",
	} {
		//nolint:gosec // G204: a fixed printf, with the quoted value under test.
		out, err := exec.Command("sh", "-c", "printf %s "+shellQuote(s)).Output()
		require.NoError(t, err, "%q", s)
		assert.Equal(t, s, string(out), "%q quoted as %s", s, shellQuote(s))
	}
	assert.True(t, strings.HasPrefix(shellQuote("=cmd"), "'"), "zsh expands a leading =")
}

// The event help's example, deployment.started, would record deployment.started.started.
func TestRunHelpShowsABareType(t *testing.T) {
	code, _, errOut := record(t, "http://x", "run", "-h")
	assert.Equal(t, 2, code)
	assert.Contains(t, errOut, "-type")
	assert.NotContains(t, errOut, "deployment.started")
}

// Under --strict a pipeline insists on the record.
func TestStrictFailsWhenTheEndIsNotConfirmed(t *testing.T) {
	shortWindows(t)
	f := &fakeIngress{}
	failEnd := &endFailingIngress{inner: f}
	code, _, errOut := record(t, failEnd.server(t, f.server(t)), "run", "--strict",
		"--type", "deployment", "--subject", "acme/prod", "--", "true")
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "horo event ", "the close command is still printed")
}

func TestRunSendsTitleActorAndAttributesOnBothPosts(t *testing.T) {
	f := &fakeIngress{}
	code, _, _ := runCmd(t, f.server(t), "--title", "Deploy api", "--actor", "ci-bot",
		"--attr", "team=sre", "--", "sh", "-c", "exit 3")
	require.Equal(t, 3, code)
	posts := f.posted()
	require.Len(t, posts, 2)
	for _, p := range posts {
		assert.Equal(t, "Deploy api", p["title"], "%s", p["type"])
		assert.Equal(t, "ci-bot", p["actor"], "%s", p["type"])
	}
	assert.Equal(t, map[string]any{"team": "sre"}, posts[0]["attributes"])
	assert.Equal(t, map[string]any{"team": "sre", "exit_code": float64(3)}, posts[1]["attributes"],
		"the end carries --attr as well as its outcome")
}

// Pasting the close command after a stored cancellation must replay, not add a second end.
func TestAnAbortsCancellationUsesTheEndKey(t *testing.T) {
	shortWindows(t)
	keys := make(chan string, 4)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		k, _ := body["idempotency_key"].(string)
		keys <- k
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	done := make(chan string, 1)
	go func() {
		_, _, errOut := runCmd(t, srv.URL, "--", "true")
		done <- errOut
	}()
	start := <-keys
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
	cancel := <-keys
	errOut := <-done
	want := strings.TrimSuffix(start, "/start") + "/end"
	assert.Equal(t, want, cancel)
	assert.Contains(t, errOut, "--idempotency-key "+want, "the close command carries the same key")
}

// The promises docs/reference/horo.md and the decision record make.
func TestTheRecordingWindowsAreTheDocumentedOnes(t *testing.T) {
	assert.Equal(t, 10*time.Second, startWindow)
	assert.Equal(t, 30*time.Second, endWindow)
	assert.Equal(t, 3*time.Second, abortWindow)
}
