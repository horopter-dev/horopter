package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reportFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "report.json")
}

func readReport(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: the test's own temporary file.
	require.NoError(t, err, "the report must be written")
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	return got
}

func TestTheReportOfARecordedRun(t *testing.T) {
	path := reportFile(t)
	code, _, _ := runCmd(t, (&fakeIngress{}).server(t), "--ref", "dep-1", "--report", path,
		"--", "true")
	require.Equal(t, 0, code)
	assert.Equal(t, map[string]any{
		"start_posted": true, "end_posted": true, "ref": "ci/dep-1", "producer_ref": "dep-1",
		"outcome": "completed", "exit_code": float64(0), "reason": nil,
	}, readReport(t, path))
}

func TestTheReportWhenHoropterIsDown(t *testing.T) {
	shortWindows(t)
	path := reportFile(t)
	code, _, _ := runCmd(t, "http://127.0.0.1:1", "--ref", "dep-1", "--report", path,
		"--", "sh", "-c", "exit 4")
	require.Equal(t, 4, code)
	got := readReport(t, path)
	assert.Equal(t, false, got["start_posted"])
	assert.Equal(t, false, got["end_posted"])
	assert.Nil(t, got["ref"], "nothing was stored")
	assert.Equal(t, "dep-1", got["producer_ref"])
	assert.Equal(t, "failed", got["outcome"], "the command ran, and failed")
	assert.Equal(t, float64(4), got["exit_code"])
	assert.Contains(t, got["reason"], "could not record the start")
}

// The run with no key is the one a CI wrapper most needs to hear about.
func TestTheVariableAsksForTheReportWithoutAKey(t *testing.T) {
	for _, tc := range []struct {
		strict  bool
		code    int
		outcome any
	}{
		{strict: false, code: 5, outcome: "failed"},
		{strict: true, code: 1, outcome: nil},
	} {
		t.Run(fmt.Sprintf("strict %v", tc.strict), func(t *testing.T) {
			path := reportFile(t)
			env := map[string]string{"HOROPTER_INGRESS_URL": "http://127.0.0.1:1",
				"HORO_RUN_REPORT": path}
			args := []string{"run", "--type", "deployment", "--subject", "s", "--ref", "dep-1"}
			if tc.strict {
				args = append(args, "--strict")
			}
			var out, errOut bytes.Buffer
			code := run(append(args, "--", "sh", "-c", "exit 5"),
				func(k string) string { return env[k] }, &out, &errOut, false)
			require.Equal(t, tc.code, code)
			assert.Equal(t, map[string]any{
				"start_posted": false, "end_posted": false, "ref": nil, "producer_ref": "dep-1",
				"outcome": tc.outcome, "exit_code": float64(tc.code),
				"reason": "no key: set --key or HOROPTER_INGRESS_KEY to a producer key",
			}, readReport(t, path))
		})
	}
}

func TestTheReportIsWrittenWhenTheEndPostFails(t *testing.T) {
	shortWindows(t)
	for _, tc := range []struct {
		strict bool
		code   int
	}{{strict: false, code: 0}, {strict: true, code: 1}} {
		t.Run(fmt.Sprintf("strict %v", tc.strict), func(t *testing.T) {
			f := &fakeIngress{}
			path := reportFile(t)
			args := []string{"--ref", "dep-1", "--report", path}
			if tc.strict {
				args = append(args, "--strict")
			}
			code, _, _ := runCmd(t, (&endFailingIngress{inner: f}).server(t, f.server(t)),
				append(args, "--", "true")...)
			require.Equal(t, tc.code, code)
			got := readReport(t, path)
			assert.Equal(t, true, got["start_posted"])
			assert.Equal(t, false, got["end_posted"])
			assert.Equal(t, "ci/dep-1", got["ref"])
			assert.Equal(t, "completed", got["outcome"])
			assert.Equal(t, float64(tc.code), got["exit_code"])
			assert.Contains(t, got["reason"], "could not confirm the end was recorded")
			assert.Contains(t, got["reason"], "end refused")
		})
	}
}

// interruptedStart interrupts horo during a held start post; cancelStatus answers the cancel.
func interruptedStart(t *testing.T, cancelStatus int) (int, map[string]any) {
	t.Helper()
	shortWindows(t)
	starts := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["lifecycle"] == "started" {
			starts <- struct{}{}
			<-release
			return
		}
		w.WriteHeader(cancelStatus)
		_, _ = w.Write([]byte(`{"ref":"ci/dep-1","created":true,"error":"cancel refused"}`))
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	path := reportFile(t)
	done := make(chan int, 1)
	go func() {
		code, _, _ := runCmd(t, srv.URL, "--ref", "dep-1", "--report", path, "--", "true")
		done <- code
	}()
	<-starts
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
	select {
	case code := <-done:
		return code, readReport(t, path)
	case <-time.After(5 * time.Second):
		t.Fatal("horo run did not return after the interrupt")
		return 0, nil
	}
}

// A confirmed cancellation carries the stored ref; an unconfirmed one says it may be open.
func TestTheReportOfAnInterruptedStart(t *testing.T) {
	t.Run("cancellation confirmed", func(t *testing.T) {
		code, got := interruptedStart(t, http.StatusCreated)
		assert.Equal(t, 128+int(syscall.SIGINT), code)
		assert.Equal(t, map[string]any{
			"start_posted": false, "end_posted": true, "ref": "ci/dep-1", "producer_ref": "dep-1",
			"outcome": "cancelled", "exit_code": float64(130),
			"reason": "interrupted before the command started",
		}, got)
	})
	t.Run("cancellation refused", func(t *testing.T) {
		code, got := interruptedStart(t, http.StatusBadRequest)
		assert.Equal(t, 128+int(syscall.SIGINT), code)
		assert.Equal(t, false, got["end_posted"])
		assert.Nil(t, got["ref"])
		assert.Equal(t, "cancelled", got["outcome"])
		assert.Contains(t, got["reason"], "interrupted before the command started")
		assert.Contains(t, got["reason"], "the cancellation could not be confirmed")
		assert.Contains(t, got["reason"], "cancel refused")
	})
}

// The outcome is how the command ended, whether or not Horopter recorded it.
func TestTheReportCarriesEachOutcome(t *testing.T) {
	shortWindows(t)
	for _, tc := range []struct {
		script, outcome string
		code            int
	}{
		{"exit 0", "completed", 0},
		{"exit 3", "failed", 3},
		{"kill -TERM $$", "cancelled", 128 + int(syscall.SIGTERM)},
	} {
		for _, up := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/ingress up %v", tc.outcome, up), func(t *testing.T) {
				url := "http://127.0.0.1:1"
				if up {
					url = (&fakeIngress{}).server(t)
				}
				path := reportFile(t)
				code, _, _ := runCmd(t, url, "--report", path, "--", "sh", "-c", tc.script)
				require.Equal(t, tc.code, code)
				got := readReport(t, path)
				assert.Equal(t, tc.outcome, got["outcome"])
				assert.Equal(t, float64(tc.code), got["exit_code"])
				assert.Equal(t, up, got["start_posted"])
				assert.Equal(t, up, got["end_posted"])
			})
		}
	}
}

// Ingress refused the start, so no change was opened and none was closed.
func TestTheReportOfAStrictRefusalHasNoOutcome(t *testing.T) {
	shortWindows(t)
	path := reportFile(t)
	url := (&fakeIngress{status: http.StatusUnauthorized}).server(t)
	code, _, _ := runCmd(t, url, "--strict", "--report", path, "--", "true")
	require.Equal(t, 1, code)
	got := readReport(t, path)
	assert.Nil(t, got["outcome"], "the command was not run")
	assert.Equal(t, false, got["start_posted"])
	assert.Equal(t, float64(1), got["exit_code"])
	assert.Contains(t, got["reason"], "could not record the start")
}

// Recording never blocks the work, and the report is part of recording.
func TestAReportThatCannotBeWrittenLeavesTheExitCodeAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "report.json")
	code, _, errOut := runCmd(t, (&fakeIngress{}).server(t), "--report", path,
		"--", "sh", "-c", "exit 3")
	assert.Equal(t, 3, code)
	assert.Contains(t, errOut, "could not write the report")
	assert.Contains(t, errOut, path)
}

func TestNoReportIsWrittenUnlessAskedFor(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	code, _, _ := runCmd(t, (&fakeIngress{}).server(t), "--", "true")
	require.Equal(t, 0, code)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
