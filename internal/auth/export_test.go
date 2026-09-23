package auth

import "horopter.dev/horopter/internal/core"

// MaxCacheEntries exposes maxCacheEntries.
const MaxCacheEntries = maxCacheEntries

// TenantFrom exposes tenantFrom, which is where a NULL tenant_id becomes a global
// credential and nothing else is allowed to.
func TenantFrom(id *string) (core.TenantID, error) { return tenantFrom(id) }
