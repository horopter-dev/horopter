package auth

import (
	"time"

	"horopter.dev/horopter/internal/core"
)

// MaxCacheEntries exposes maxCacheEntries.
const MaxCacheEntries = maxCacheEntries

// LookupTimeoutOf returns the bound every store call runs under, which is where an
// omitted Options.LookupTimeout becomes the default rather than "unbounded".
func LookupTimeoutOf(a *Authenticator) time.Duration { return a.timeout }

// TenantFrom exposes tenantFrom, which is where a NULL tenant_id becomes a global
// credential and nothing else is allowed to.
func TenantFrom(id *string) (core.TenantID, error) { return tenantFrom(id) }
