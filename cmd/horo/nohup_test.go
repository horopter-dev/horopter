package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain lets a test run this binary as horo, started with signals already ignored.
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

// Catching SIGHUP would un-ignore it, and exec would give the command the default action.
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

// Only a foreground job gets a terminal's Ctrl-C, so only it may skip forwarding SIGINT.
func TestABackgroundJobOnATerminalIsNotInTheForeground(t *testing.T) {
	_, err := exec.LookPath("script")
	require.NoError(t, err, "this test needs script(1) for a pty: util-linux on Linux, "+
		"the base system on macOS; it is not skipped, since nothing else pins inForeground")
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
