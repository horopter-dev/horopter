// Package separation holds no code: its tests keep this repository from citing the private one.
package separation

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// privateRepository is assembled so that this file does not cite it.
const privateRepository = "horopter" + "-internal"

// exempt are the agent files, which point at the private repository's sibling checkout.
var exempt = []string{
	"CLAUDE.md",
	"docs/agents/",
}

// citations git-greps the working tree at dir for the private repository, outside exempt.
func citations(dir string) ([]string, error) {
	args := []string{"grep", "--line-number", "-I", "--fixed-strings", privateRepository, "--"}
	for _, path := range exempt {
		args = append(args, ":(exclude)"+path)
	}
	out, err := git(dir, args...).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil, nil // git grep's exit status when nothing matches
	}
	if err != nil {
		return nil, fmt.Errorf("git grep in %s: %w", dir, err)
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n"), nil
}

// git drops inherited GIT_* variables: under a hook, GIT_INDEX_FILE overrides -C.
func git(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: git.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	return cmd
}

func TestNoTrackedFileCitesThePrivateRepository(t *testing.T) {
	found, err := citations(filepath.Join("..", ".."))
	require.NoError(t, err)
	if len(found) > 0 {
		t.Fatalf("these lines name %s, whose documents a reader cannot reach; write the "+
			"reasoning in place instead:\n%s", privateRepository, strings.Join(found, "\n"))
	}
}

func TestCitationsFindsOnlyTrackedFilesThatAreNotExempt(t *testing.T) {
	dir := repository(t, map[string]string{
		"internal/core/ref.go":        "// " + privateRepository + " has the design.\n",
		"CLAUDE.md":                   "Specs are in " + privateRepository + ".\n",
		"docs/agents/domain.md":       "../" + privateRepository + "/CONTEXT.md\n",
		"reviews/2026-10-06-other.md": "line one\n" + privateRepository + "\n",
		"README.md":                   "Nothing private here.\n",
	})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked.go"),
		[]byte(privateRepository), 0o600))

	found, err := citations(dir)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		"internal/core/ref.go:1:// " + privateRepository + " has the design.",
		"reviews/2026-10-06-other.md:2:" + privateRepository,
	}, found)
}

func TestCitationsIsEmptyWhenNothingNamesIt(t *testing.T) {
	dir := repository(t, map[string]string{"README.md": "clean\n"})

	found, err := citations(dir)
	require.NoError(t, err)
	require.Empty(t, found)
}

func TestCitationsIgnoresBinaryFiles(t *testing.T) {
	dir := repository(t, map[string]string{"logo.png": "\x00" + privateRepository})

	found, err := citations(dir)
	require.NoError(t, err)
	require.Empty(t, found)
}

func TestCitationsFailsOutsideARepository(t *testing.T) {
	_, err := citations(t.TempDir())
	require.ErrorContains(t, err, "git grep in")
}

func TestCitationsIgnoresAnInheritedIndex(t *testing.T) {
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "absent", "index"))
	dir := repository(t, map[string]string{"main.go": "// " + privateRepository + "\n"})

	found, err := citations(dir)
	require.NoError(t, err)
	require.Len(t, found, 1)
}

// repository makes a git repository in a temporary directory with files tracked.
func repository(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	run(t, git(dir, "init", "--quiet"))
	for path, content := range files {
		full := filepath.Join(dir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
		run(t, git(dir, "add", path))
	}
	return dir
}

func run(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s: %s", strings.Join(cmd.Args, " "), out)
}
