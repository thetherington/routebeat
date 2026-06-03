package cache

import (
	"encoding/gob"
	"os"
	"sync"
	"time"
)

// generic, thread-safe cache

type cacheItem[V any] struct {
	Value     V
	ExpiresAt time.Time // zero means no expiry
}

type CacheMap[K comparable, V any] struct {
	mu     sync.RWMutex
	Store  map[K]cacheItem[V]
	expiry time.Duration
}

// NewCacheMap initializes and returns a new cache with a given expiry window
func NewCacheMap[K comparable, V any](expiry time.Duration) *CacheMap[K, V] {
	return &CacheMap[K, V]{
		Store:  make(map[K]cacheItem[V]),
		expiry: expiry,
	}
}

// Clear cache by reinitializing the Store
func (c *CacheMap[K, V]) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.Store = make(map[K]cacheItem[V])
}

// Get retrieves a value from the cache and a boolean if found and not expired
func (c *CacheMap[K, V]) Get(key K) (V, bool) {
	c.mu.RLock()
	item, ok := c.Store[key]
	c.mu.RUnlock()

	if !ok {
		var zero V
		return zero, false
	}
	if !item.ExpiresAt.IsZero() && time.Now().After(item.ExpiresAt) {
		// expired, delete and return not found
		c.mu.Lock()
		delete(c.Store, key)
		c.mu.Unlock()
		var zero V
		return zero, false
	}
	return item.Value, true
}

// Set adds or updates a value in the cache, with expiry
func (c *CacheMap[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var expiresAt time.Time
	if c.expiry > 0 {
		expiresAt = time.Now().Add(c.expiry)
	}
	c.Store[key] = cacheItem[V]{
		Value:     value,
		ExpiresAt: expiresAt,
	}
}

// Has checks if a key exists in the cache and is not expired
func (c *CacheMap[K, V]) Has(key K) bool {
	c.mu.RLock()
	item, ok := c.Store[key]
	c.mu.RUnlock()
	if !ok {
		return false
	}
	if !item.ExpiresAt.IsZero() && time.Now().After(item.ExpiresAt) {
		c.mu.Lock()
		delete(c.Store, key)
		c.mu.Unlock()
		return false
	}
	return true
}

// Length returns the number of non-expired items in the cache
func (c *CacheMap[K, V]) Length() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	now := time.Now()
	for _, item := range c.Store {
		if item.ExpiresAt.IsZero() || now.Before(item.ExpiresAt) {
			n++
		}
	}
	return n
}

// Delete removes a key from the cache
func (c *CacheMap[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.Store, key)
}

// Load replaces the Store with a new map (with no expiry)
func (c *CacheMap[K, V]) Load(m map[K]V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	newStore := make(map[K]cacheItem[V], len(m))
	for k, v := range m {
		newStore[k] = cacheItem[V]{Value: v}
	}
	c.Store = newStore
}

// Do runs a custom function with exclusive access to the cache
func (c *CacheMap[K, V]) Do(fn func(c *CacheMap[K, V])) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

// Do executes a function on the value for a given key if it exists and not expired
func (c *CacheMap[K, V]) DoMut(key K, fn func(value V)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if item, ok := c.Store[key]; ok {
		if item.ExpiresAt.IsZero() || time.Now().Before(item.ExpiresAt) {
			fn(item.Value)
		} else {
			delete(c.Store, key)
		}
	}
}

// SaveToFile persists the cache map to disk using gob (only non-expired items)
func (c *CacheMap[K, V]) SaveToFile(filename string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	enc := gob.NewEncoder(file)
	now := time.Now()
	cleanStore := make(map[K]cacheItem[V])
	for k, item := range c.Store {
		if item.ExpiresAt.IsZero() || now.Before(item.ExpiresAt) {
			cleanStore[k] = item
		}
	}
	return enc.Encode(cleanStore)
}

// LoadFromFile loads the cache map from disk using gob
func (c *CacheMap[K, V]) LoadFromFile(filename string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	dec := gob.NewDecoder(file)
	return dec.Decode(&c.Store)
}

// Cleanup removes all expired keys from the cache
func (c *CacheMap[K, V]) Cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, item := range c.Store {
		if !item.ExpiresAt.IsZero() && now.After(item.ExpiresAt) {
			delete(c.Store, k)
		}
	}
}
