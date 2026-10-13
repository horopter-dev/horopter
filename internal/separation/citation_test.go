// Package separation holds no code: its tests keep this repository from citing the private
// repository its specs and decisions live in, whose documents a reader of the public code
// cannot reach.
package separation

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// privateRepository is assembled so that this file does not cite it.
const privateRepository = "horopter" + "-internal"

// mayNameIt are the files allowed to name the private repository. The agent files point at
// its sibling checkout. The mutation battery review's title names one of its issues, kept
// until the exposure audit decides whether it stays.
var mayNameIt = []string{
	"CLAUDE.md",
	"docs/agents/",
	"reviews/2026-10-05-mutation-battery.md",
}

// citations returns every line of a file tracked in the repository at dir that names the
// private repository, as git grep prints it, except in the files that may.
func citations(t *testing.T, dir string) []string {
	t.Helper()
	args := []string{"-C", dir, "grep", "--line-number", "-I", "--fixed-strings",
		privateRepository, "--"}
	for _, path := range mayNameIt {
		args = append(args, ":(exclude)"+path)
	}
	out, err := exec.Command("git", args...).Output() //nolint:gosec // G204: git, fixed args.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil // git grep's exit status when nothing matches
	}
	require.NoError(t, err, "git grep in %s", dir)
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

func TestNoTrackedFileCitesThePrivateRepository(t *testing.T) {
	if found := citations(t, filepath.Join("..", "..")); len(found) > 0 {
		t.Fatalf("these lines name %s, whose documents a reader cannot reach; write the "+
			"reasoning in place instead:\n%s", privateRepository, strings.Join(found, "\n"))
	}
}

func TestCitationsFindsOnlyTrackedFilesThatMayNotNameIt(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"internal/core/ref.go":                   "// " + privateRepository + " has the design.\n",
		"CLAUDE.md":                              "Specs are in " + privateRepository + ".\n",
		"docs/agents/domain.md":                  "../" + privateRepository + "/CONTEXT.md\n",
		"reviews/2026-10-05-mutation-battery.md": "# " + privateRepository + " #29\n",
		"reviews/2026-10-06-other.md":            "line one\n" + privateRepository + "\n",
		"README.md":                              "Nothing private here.\n",
	}
	for path, content := range files {
		full := filepath.Join(dir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked.go"),
		[]byte(privateRepository), 0o600))
	git(t, dir, "init", "--quiet")
	for path := range files {
		git(t, dir, "add", path)
	}

	require.ElementsMatch(t, []string{
		"internal/core/ref.go:1:// " + privateRepository + " has the design.",
		"reviews/2026-10-06-other.md:2:" + privateRepository,
	}, citations(t, dir))
}

func TestCitationsIsEmptyWhenNothingNamesIt(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("clean\n"), 0o600))
	git(t, dir, "init", "--quiet")
	git(t, dir, "add", "README.md")

	require.Empty(t, citations(t, dir))
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	//nolint:gosec // G204: git, fixed args.
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
}
