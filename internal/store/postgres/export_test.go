package postgres

// SetAfterLoadHook installs fn as the hook applyToProjection calls between its locked
// read of a change and the write of the folded result, or clears it when fn is nil.
//
// This file is part of the test binary only, so afterLoadHook stays unreachable from
// outside the package in a normal build.
func SetAfterLoadHook(fn func()) { afterLoadHook = fn }

// SetAfterIdemLookupHook installs fn as the hook AppendEvent calls after an
// idempotency-key lookup that found nothing and before the event insert, or clears it
// when fn is nil. It is what lets a test hold two appends open across that window so
// both reach the insert, which is the only way to exercise the real unique-violation
// recovery rather than a faked error.
func SetAfterIdemLookupHook(fn func()) { afterIdemLookupHook = fn }

// SetAfterDeleteHook installs fn as the hook RebuildProjection calls between clearing a
// tenant's projection and re-inserting it, or clears it when fn is nil. Holding a
// rebuild open there is how a test puts an append into exactly the window the
// tenant-scoped advisory lock exists to close.
func SetAfterDeleteHook(fn func()) { afterDeleteHook = fn }
