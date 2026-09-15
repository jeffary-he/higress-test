package whitelist

import (
	"sync"
	"time"
)

type Cache struct {
	mu          sync.RWMutex
	entries     map[string]struct{}
	loaded      bool
	lastSuccess time.Time
	ttl         time.Duration
	generation  uint64
	busy        bool
	deadline    time.Time
}

func NewCache(ttl time.Duration) *Cache { return &Cache{ttl: ttl} }

func (c *Cache) Contains(digest string, now time.Time) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.loaded || now.Before(c.lastSuccess) || now.Sub(c.lastSuccess) >= c.ttl {
		return false
	}
	_, ok := c.entries[digest]
	return ok
}

// Retries expired refreshes; a generation ID protects against delayed callbacks.
func (c *Cache) Begin(now time.Time, timeout time.Duration) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy && now.Before(c.deadline) {
		return 0, false
	}
	c.generation++
	c.busy = true
	c.deadline = now.Add(timeout)
	return c.generation, true
}

func (c *Cache) Finish(id uint64, started, now time.Time, entries map[string]struct{}, success bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id != c.generation || !c.busy {
		return false
	}
	c.busy = false
	if !success || now.Before(started) || !now.Before(c.deadline) {
		return false
	}
	// Caller transfers ownership of the completely validated immutable snapshot.
	c.entries = entries
	c.lastSuccess = now
	c.loaded = true
	return true
}
