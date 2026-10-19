package buildinfo_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// A binary added without buildinfo.Handle fails here rather than on someone's laptop.
func TestEveryBinaryAnswers(t *testing.T) {
	cmds, err := os.ReadDir(filepath.Join("..", "..", "cmd"))
	require.NoError(t, err)
	bin := t.TempDir()
	//nolint:gosec // G204: bin is this test's own temporary directory.
	build := exec.Command("go", "build", "-o", bin+string(os.PathSeparator), "../../cmd/...")
	out, err := build.CombinedOutput()
	require.NoError(t, err, "%s", out)
	for _, c := range cmds {
		if !c.IsDir() {
			continue
		}
		t.Run(c.Name(), func(t *testing.T) {
			path := filepath.Join(bin, c.Name())
			out := answer(t, path, "--version")
			assert.Regexp(t, `^`+c.Name()+` \S+\n$`, out)
			var info map[string]any
			require.NoError(t, json.Unmarshal([]byte(answer(t, path, "--build-info", "--json")), &info))
			assert.Equal(t, c.Name(), info["name"])
		})
	}
}

// answer runs a built binary with no environment, so the developer's shell cannot satisfy it.
func answer(t *testing.T, path string, args ...string) string {
	t.Helper()
	//nolint:gosec // G204: path is a binary this test just built.
	cmd := exec.Command(path, args...)
	cmd.Env = []string{}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	require.NoError(t, cmd.Run(), "%s %v; stderr: %s", path, args, errOut.String())
	return out.String()
}

// A GoReleaser build without the version ldflags reports a pseudo-version.
func TestEveryBinaryIsStamped(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	require.NoError(t, err)
	var config struct {
		Builds []struct {
			Main    string   `json:"main"`
			LDFlags []string `json:"ldflags"`
		} `json:"builds"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &config))
	stamped := map[string]bool{}
	for _, b := range config.Builds {
		for _, f := range b.LDFlags {
			if strings.Contains(f, "-X horopter.dev/horopter/internal/buildinfo.version={{ .Version }}") {
				stamped[filepath.Base(b.Main)] = true
			}
		}
	}
	cmds, err := os.ReadDir(filepath.Join("..", "..", "cmd"))
	require.NoError(t, err)
	for _, c := range cmds {
		if c.IsDir() {
			assert.True(t, stamped[c.Name()], "no GoReleaser build stamps cmd/%s", c.Name())
		}
	}
}
