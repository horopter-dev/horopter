package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain lets a test run this binary as horo itself, so it can start horo with signals
// already ignored, as nohup and background jobs do.
func TestMain(m *testing.M) {
	if os.Getenv("HORO_TEST_AS_HORO") == "1" {
		main()
	}
	if os.Getenv("HORO_TEST_PRINT_FOREGROUND") == "1" {
		fmt.Println(inForeground("/dev/tty"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestNohupStillProtectsTheCommand: nohup starts horo with SIGHUP ignored. Catching it would
// un-ignore it, exec would reset it to the default, and the wrapped command would die on a
// hangup — the opposite of what nohup is for.
func TestNohupStillProtectsTheCommand(t *testing.T) {
	f := &fakeIngress{}
	url := f.server(t)
	self, err := os.Executable()
	require.NoError(t, err)
	script := `trap "" HUP; exec "$0" run --type deployment --subject s -- ` +
		`sh -c 'kill -HUP $$; echo survived'`
	//nolint:gosec // G204: runs this test binary as horo, under a fixed script.
	cmd := exec.Command("sh", "-c", script, self)
	cmd.Env = append(os.Environ(), "HORO_TEST_AS_HORO=1",
		"HOROPTER_INGRESS_URL="+url, "HOROPTER_INGRESS_KEY=pk")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "horo run must exit 0: %s", out)
	assert.True(t, strings.Contains(string(out), "survived"), "the command must survive: %s", out)
	assert.Equal(t, "deployment.completed", f.posted()[1]["type"])
}

// TestSignalsAreNotCaughtWhenAllFourWereInheritedIgnored: with INT, TERM, HUP and QUIT all
// inherited as ignored, notify has nothing to register, and signal.Notify with no signals
// means every signal. horo would then catch and forward signals it never meant to — here a
// SIGWINCH the wrapped command sends horo comes back to it.
func TestSignalsAreNotCaughtWhenAllFourWereInheritedIgnored(t *testing.T) {
	f := &fakeIngress{}
	url := f.server(t)
	self, err := os.Executable()
	require.NoError(t, err)
	child := filepath.Join(t.TempDir(), "child.sh")
	require.NoError(t, os.WriteFile(child, []byte(
		"trap 'echo got-winch' WINCH\nkill -WINCH \"$PPID\"\nsleep 0.5\necho done\n"), 0o600))
	script := `trap "" INT TERM HUP QUIT; exec "$0" run --type deployment --subject s -- sh "$1"`
	//nolint:gosec // G204: runs this test binary as horo, under a fixed script.
	cmd := exec.Command("sh", "-c", script, self, child)
	cmd.Env = append(os.Environ(), "HORO_TEST_AS_HORO=1",
		"HOROPTER_INGRESS_URL="+url, "HOROPTER_INGRESS_KEY=pk")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	assert.Contains(t, string(out), "done")
	assert.NotContains(t, string(out), "got-winch", "horo must not have caught and forwarded WINCH")
}

// TestABackgroundJobOnATerminalIsNotInTheForeground runs this binary in a pty through
// script(1), once in the foreground and once as a job-controlled background job. Only the
// foreground job receives a terminal's Ctrl-C, so only it may skip forwarding SIGINT; a
// background horo that thought otherwise would never pass a kill -INT on to its command.
func TestABackgroundJobOnATerminalIsNotInTheForeground(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	for name, inner := range map[string]string{
		"foreground": `"$0"`,
		"background": `set -m; "$0" & wait`,
	} {
		t.Run(name, func(t *testing.T) {
			shell := fmt.Sprintf("sh -c '%s' %s", inner, self)
			args := []string{"-q", "/dev/null", "sh", "-c", shell}
			if runtime.GOOS == "linux" {
				args = []string{"-qec", shell, "/dev/null"}
			}
			//nolint:gosec // G204: script(1) running this test binary under a fixed shell line.
			cmd := exec.Command("script", args...)
			cmd.Env = append(os.Environ(), "HORO_TEST_PRINT_FOREGROUND=1")
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
			want := map[string]string{"foreground": "true", "background": "false"}[name]
			assert.Contains(t, string(out), want, "%s", out)
		})
	}
}
