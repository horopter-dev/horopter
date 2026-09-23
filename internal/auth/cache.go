package auth

import (
	"sync"
	"time"
)

// maxCacheEntries bounds one generation of the credential cache. A flood of distinct
// unknown prefixes writes a negative entry each, so without a bound the cache that
// exists to absorb such a flood becomes the memory it exhausts instead.
const maxCacheEntries = 8192

// entry is one lookup outcome. found distinguishes a cached credential from a cached
// absence, which is the whole point of caching a miss.
type entry struct {
	cred    Credential
	found   bool
	expires time.Time
}

// cache holds lookup outcomes by key prefix. It keeps two generations rather than
// sweeping expired entries: a full generation is retired whole and dropped on the next
// rotation, which bounds memory without an O(n) pass on the request path.
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

// get returns the outcome cached for prefix, if one is present and still live at now.
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

// put caches e under prefix for the TTL its outcome carries; a TTL of zero caches
// nothing, which is how the cache is turned off.
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
