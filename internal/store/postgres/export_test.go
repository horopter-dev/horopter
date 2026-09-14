package postgres

// SetAfterLoadHook installs fn as the hook applyToProjection calls between its locked
// read of a change and the write of the folded result, or clears it when fn is nil.
//
// This file is part of the test binary only, so afterLoadHook stays unreachable from
// outside the package in a normal build.
func SetAfterLoadHook(fn func()) { afterLoadHook = fn }
