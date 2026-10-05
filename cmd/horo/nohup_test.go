package main

import (
	"os"
	"os/exec"
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
