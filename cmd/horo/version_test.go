package main

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

func buildInfo(v string) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: v}}, true
	}
}

func TestVersionPrintsTheStampedRelease(t *testing.T) {
	stamped := version
	version = "v0.9.0"
	t.Cleanup(func() { version = stamped })
	code, out, errOut := horo(t, "", false, "--version")
	assert.Equal(t, 0, code)
	assert.Equal(t, "horo v0.9.0\n", out)
	assert.Empty(t, errOut)
}

func TestVersionTakesNoArguments(t *testing.T) {
	code, out, errOut := horo(t, "", false, "--version", "changes")
	assert.Equal(t, 2, code)
	assert.Empty(t, out)
	assert.Contains(t, errOut, "--version takes no arguments")
}

func TestAStampedReleaseWinsOverTheModuleVersion(t *testing.T) {
	assert.Equal(t, "v0.9.0", versionOf("v0.9.0", buildInfo("v0.0.0-20261011-abcdef")))
}

func TestGoInstallReportsTheModuleVersion(t *testing.T) {
	assert.Equal(t, "v0.9.0", versionOf("", buildInfo("v0.9.0")))
}

func TestWithoutAModuleVersionTheVersionIsUnknown(t *testing.T) {
	none := func() (*debug.BuildInfo, bool) { return nil, false }
	assert.Equal(t, "unknown", versionOf("", none), "no build info")
	assert.Equal(t, "unknown", versionOf("", buildInfo("")), "build info without a version")
}
