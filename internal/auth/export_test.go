package auth

import (
	"time"

	"horopter.dev/horopter/internal/core"
)

// MaxCacheEntries exposes maxCacheEntries.
const MaxCacheEntries = maxCacheEntries

// LookupTimeoutOf returns the bound every store call runs under.
func LookupTimeoutOf(a *Authenticator) time.Duration { return a.timeout }

// TenantFrom exposes tenantFrom.
func TenantFrom(id *string) (core.TenantID, error) { return tenantFrom(id) }
