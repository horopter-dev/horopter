package buildinfo

import (
	"bytes"
	"encoding/json"
	"errors"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stamp sets what the linker would have stamped and what the build info holds, for one test.
func stamp(t *testing.T, stamped string, info *debug.BuildInfo) {
	t.Helper()
	oldVersion, oldRead := version, readBuildInfo
	t.Cleanup(func() { version, readBuildInfo = oldVersion, oldRead })
	version = stamped
	readBuildInfo = func() (*debug.BuildInfo, bool) { return info, info != nil }
}

func vcsBuild(moduleVersion string) *debug.BuildInfo {
	return &debug.BuildInfo{
		Main: debug.Module{Version: moduleVersion},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "d3c267462ed82a96452c5b820fc8a8d3c91c2391"},
			{Key: "vcs.time", Value: "2026-10-10T23:35:08Z"},
			{Key: "vcs.modified", Value: "false"},
		},
	}
}

func handle(args ...string) (int, bool, string, string) {
	var out, errOut bytes.Buffer
	code, handled := Handle("horo", args, &out, &errOut)
	return code, handled, out.String(), errOut.String()
}

func TestAStampedReleaseWinsOverTheModuleVersion(t *testing.T) {
	stamp(t, "0.9.0", vcsBuild("v0.0.0-20261011-abcdef"))
	assert.Equal(t, "0.9.0", Version())
}

func TestGoInstallReportsTheModuleVersionWithoutItsV(t *testing.T) {
	stamp(t, "", vcsBuild("v0.9.0"))
	assert.Equal(t, "0.9.0", Version())
}

func TestTheTextOutputAloneAddsTheV(t *testing.T) {
	cases := []struct{ moduleVersion, want string }{
		{"v0.9.0", "horo v0.9.0\n"},
		{"v0.9.1-0.20261011143000-abcdef+dirty", "horo v0.9.1-0.20261011143000-abcdef+dirty\n"},
		{"(devel)", "horo (devel)\n"},
	}
	for _, c := range cases {
		stamp(t, "", vcsBuild(c.moduleVersion))
		_, _, out, _ := handle("--version")
		assert.Equal(t, c.want, out, c.moduleVersion)
	}
	stamp(t, "", nil)
	_, _, out, _ := handle("--version")
	assert.Equal(t, "horo unknown\n", out)
}

func TestWithoutAModuleVersionTheVersionIsUnknown(t *testing.T) {
	stamp(t, "", nil)
	assert.Equal(t, "unknown", Version(), "no build info")
	stamp(t, "", vcsBuild(""))
	assert.Equal(t, "unknown", Version(), "build info without a version")
}

func TestOtherArgumentsAreNotHandled(t *testing.T) {
	for _, args := range [][]string{nil, {"changes", "--version"}, {"-addr", ":8080"}} {
		code, handled, out, errOut := handle(args...)
		assert.False(t, handled, "%q", args)
		assert.Zero(t, code)
		assert.Empty(t, out+errOut)
	}
}

func TestVersionPrintsTheNameAndTheRelease(t *testing.T) {
	stamp(t, "0.9.0", nil)
	for _, flag := range []string{"--version", "-version"} {
		code, handled, out, errOut := handle(flag)
		assert.True(t, handled)
		assert.Equal(t, 0, code)
		assert.Equal(t, "horo v0.9.0\n", out)
		assert.Empty(t, errOut)
	}
}

func TestVersionAsJSON(t *testing.T) {
	stamp(t, "0.9.0", nil)
	for _, args := range [][]string{{"--version", "--json"}, {"--version", "--format", "json"}} {
		code, _, out, _ := handle(args...)
		require.Equal(t, 0, code, "%q", args)
		assert.JSONEq(t, `{"name":"horo","version":"0.9.0"}`, out)
	}
}

func TestBareIsTheVersionAlone(t *testing.T) {
	stamp(t, "0.9.0", nil)
	code, handled, out, errOut := handle("--version", "--format", "bare")
	assert.True(t, handled)
	assert.Equal(t, 0, code)
	assert.Equal(t, "0.9.0\n", out, "no name and no v, for a script to compare")
	assert.Empty(t, errOut)
	stamp(t, "", vcsBuild("(devel)"))
	_, _, out, _ = handle("--version", "--format", "bare")
	assert.Equal(t, "(devel)\n", out)
}

func TestBuildInfoReportsTheCommitAndPlatform(t *testing.T) {
	stamp(t, "0.9.0", vcsBuild("v0.9.0"))
	code, handled, out, _ := handle("--build-info")
	assert.True(t, handled)
	assert.Equal(t, 0, code)
	assert.Equal(t, "horo v0.9.0\n"+
		"Commit:      d3c267462ed82a96452c5b820fc8a8d3c91c2391\n"+
		"Commit time: 2026-10-10T23:35:08Z\n"+
		"Modified:    false\n"+
		"Go version:  "+runtime.Version()+"\n"+
		"OS:          "+runtime.GOOS+"\n"+
		"Arch:        "+runtime.GOARCH+"\n", out)
}

func TestBuildInfoAsJSON(t *testing.T) {
	stamp(t, "0.9.0", vcsBuild("v0.9.0"))
	code, _, out, _ := handle("--build-info", "--json")
	require.Equal(t, 0, code)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, map[string]any{
		"name": "horo", "version": "0.9.0",
		"commit":      "d3c267462ed82a96452c5b820fc8a8d3c91c2391",
		"commit_time": "2026-10-10T23:35:08Z", "modified": false,
		"go_version": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
	}, got)
}

func TestBuildInfoWithoutVCSOmitsWhatItCannotKnow(t *testing.T) {
	stamp(t, "0.9.0", nil)
	_, _, out, _ := handle("--build-info")
	assert.NotContains(t, out, "Commit")
	assert.NotContains(t, out, "Modified")
	assert.Contains(t, out, "Go version:")
	_, _, out, _ = handle("--build-info", "--json")
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Contains(t, got, "commit")
	assert.Nil(t, got["commit"])
	assert.Nil(t, got["modified"])
}

func TestUsageErrors(t *testing.T) {
	stamp(t, "0.9.0", nil)
	cases := map[string][]string{
		"takes no arguments":                  {"--version", "changes"},
		"use text, json or bare":              {"--version", "--format", "csv"},
		"--json or --format, not both":        {"--build-info", "--json", "--format", "text"},
		"--version or --build-info, not both": {"--version", "--build-info"},
		"--format bare goes with --version":   {"--build-info", "--format", "bare"},
		"not defined":                         {"--version", "--output", "json"},
	}
	for want, args := range cases {
		code, handled, out, errOut := handle(args...)
		assert.True(t, handled, "%q", args)
		assert.Equal(t, 2, code, "%q", args)
		assert.Empty(t, out, "%q", args)
		assert.Contains(t, errOut, want, "%q", args)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

func TestAFailedWriteIsAnError(t *testing.T) {
	stamp(t, "0.9.0", nil)
	var errOut bytes.Buffer
	code, handled := Handle("horo", []string{"--version"}, failingWriter{}, &errOut)
	assert.True(t, handled)
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "stdout closed")
}
