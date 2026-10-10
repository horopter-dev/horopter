package main

import (
	"io"
	"runtime/debug"
)

// version is the release tag GoReleaser stamps with -ldflags "-X main.version=…".
var version string

// versionOf is the stamped release, or the module version `go install …@vX.Y.Z` records.
func versionOf(stamped string, read func() (*debug.BuildInfo, bool)) string {
	if stamped != "" {
		return stamped
	}
	if info, ok := read(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}

func printVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		say(stderr, "horo: --version takes no arguments")
		return exitUsage
	}
	say(stdout, "horo", versionOf(version, debug.ReadBuildInfo))
	return exitOK
}
