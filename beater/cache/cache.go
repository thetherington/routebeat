package cache

import (
	"sync"
	"time"
)

type entry struct {
	id        string
	timestamp time.Time
}

type Cache struct {
	mu     sync.Mutex
	items  map[string]time.Time
	window time.Duration
}

func NewCache(window time.Duration) *Cache {
	return &Cache{
		items:  make(map[string]time.Time),
		window: window,
	}
}

// Add inserts an id into the cache with the current timestamp.
func (c *Cache) Add(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[id] = time.Now()
	c.purge()
}

// Exists checks if the id is in the cache and not expired.
func (c *Cache) Exists(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.items[id]
	if !ok {
		return false
	}
	if time.Since(t) > c.window {
		delete(c.items, id)
		return false
	}
	return true
}

// purge removes expired ids from the cache.
func (c *Cache) purge() {
	now := time.Now()
	for id, t := range c.items {
		if now.Sub(t) > c.window {
			delete(c.items, id)
		}
	}
}
