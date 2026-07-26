package answer

import (
	"context"
	"sync"
	"time"
)

// DefaultCacheTTL is short on purpose. Long enough that the three or four
// questions someone asks in one conversation return consistent numbers -
// "what's the burn rate?" then "and the budget?" must not disagree with
// each other - and short enough that the answer still tracks a moving
// incident.
const DefaultCacheTTL = 45 * time.Second

// Cache is a small TTL cache over tool results, keyed by intent plus
// arguments. It serves two purposes, and the second matters more than the
// first:
//
//  1. These calls hit SigNoz and are not free.
//  2. Two people asking the same question inside the same window get the
//     same numbers, and so does one person asking twice. Reliability
//     figures that shift between two adjacent messages are indistinguishable
//     from figures that were made up.
//
// It is not an LRU: entries are keyed by (intent, service, environment,
// SLO/limit) and expire in tens of seconds, so the working set is bounded
// by the number of registered services, and expired entries are reaped on
// access.
type Cache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]cacheEntry
}

type cacheEntry struct {
	value   any
	expires time.Time
}

// NewCache returns a cache with the given TTL; zero selects
// DefaultCacheTTL. A nil *Cache is valid and simply does not cache, so a
// caller that wants live reads every time can leave Deps.Cache unset.
func NewCache(ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &Cache{ttl: ttl, now: time.Now, entries: map[string]cacheEntry{}}
}

// SetClock overrides the cache's clock, so a test can assert both the hit
// and the expiry without sleeping.
func (c *Cache) SetClock(now func() time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

func (c *Cache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Cache) get(key string) (any, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !c.clock().Before(entry.expires) {
		delete(c.entries, key)
		return nil, false
	}
	return entry.value, true
}

func (c *Cache) put(key string, value any) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]cacheEntry{}
	}
	ttl := c.ttl
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	now := c.clock()
	c.entries[key] = cacheEntry{value: value, expires: now.Add(ttl)}
	// Reap on write rather than on a timer: the map only grows when new
	// keys arrive, so that is exactly when it is worth checking.
	for k, entry := range c.entries {
		if !now.Before(entry.expires) {
			delete(c.entries, k)
		}
	}
}

// Reset drops every entry. Useful when a write action has just changed
// something and the cached view is known stale.
func (c *Cache) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[string]cacheEntry{}
}

// WithCache wraps a tool so identical arguments reuse a recent result.
// The wrapper preserves the tool's types, so a cached tool remains a
// drop-in replacement for the uncached one at every typed call site.
//
// Only successful results are cached. An error is not a result, and
// caching one would turn a transient backend blip into 45 seconds of
// confident failure. An indeterminate result *is* cached: it was computed,
// it is the correct answer, and it should be as stable as any other.
func WithCache[In Args, Out any](t Tool[In, Out], cache *Cache) Tool[In, Out] {
	if cache == nil {
		return t
	}
	inner := t.fn
	t.fn = func(ctx context.Context, in In) (Envelope[Out], error) {
		key := t.toolName + "\x00" + in.CacheKey()
		if hit, ok := cache.get(key); ok {
			if env, ok := hit.(Envelope[Out]); ok {
				return env, nil
			}
		}
		env, err := inner(ctx, in)
		if err != nil {
			return env, err
		}
		// Intent is stamped by Invoke after fn returns, so stamp it here
		// too: otherwise the cached copy and the fresh copy would differ
		// in exactly one field, which is the kind of subtle inconsistency
		// a test catches long after it starts mattering.
		env.Intent = t.toolName
		cache.put(key, env)
		return env, nil
	}
	return t
}
