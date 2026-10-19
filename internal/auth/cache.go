package auth

import (
	"sync"
	"time"
)

// A flood of unknown prefixes writes an entry each, so an unbounded cache exhausts memory.
const maxCacheEntries = 8192

type entry struct {
	cred    Credential
	found   bool
	expires time.Time
}

// Two generations rather than a sweep: bounded memory without an O(n) pass per request.
type cache struct {
	mu             sync.RWMutex
	live, previous map[string]entry
	positiveTTL    time.Duration
	negativeTTL    time.Duration
}

func newCache(positiveTTL, negativeTTL time.Duration) *cache {
	return &cache{
		live:        make(map[string]entry),
		previous:    make(map[string]entry),
		positiveTTL: positiveTTL,
		negativeTTL: negativeTTL,
	}
}

func (c *cache) get(prefix string, now time.Time) (entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.live[prefix]
	if !ok {
		e, ok = c.previous[prefix]
	}
	if !ok || !now.Before(e.expires) {
		return entry{}, false
	}
	return e, true
}

func (c *cache) put(prefix string, e entry, now time.Time) {
	ttl := c.negativeTTL
	if e.found {
		ttl = c.positiveTTL
	}
	if ttl <= 0 {
		return
	}
	e.expires = now.Add(ttl)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.live) >= maxCacheEntries {
		c.previous, c.live = c.live, make(map[string]entry, maxCacheEntries)
	}
	c.live[prefix] = e
}
