package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// inForeground reports whether horo's process group is ttyPath's foreground group.
func inForeground(ttyPath string) bool {
	//nolint:gosec // G304: ttyPath is /dev/tty, or a test's own path.
	tty, err := os.Open(ttyPath)
	if err != nil {
		return false
	}
	defer func() { _ = tty.Close() }()
	pgrp, err := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP)
	return err == nil && pgrp == unix.Getpgrp()
}
