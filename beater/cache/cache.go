package cache

import (
	"encoding/gob"
	"os"
	"sync"
)

// generic, thread-safe cache
type CacheMap[K comparable, V any] struct {
	mu    sync.RWMutex
	Store map[K]V
}

// NewCacheMap initializes and returns a new cache
func NewCacheMap[K comparable, V any]() *CacheMap[K, V] {
	return &CacheMap[K, V]{
		Store: make(map[K]V),
	}
}

// Get retrieves a value from the cache and a boolean if found
func (c *CacheMap[K, V]) Get(key K) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	val, ok := c.Store[key]

	return val, ok
}

// Set adds or updates a value in the cache
func (c *CacheMap[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.Store[key] = value
}

func (c *CacheMap[K, V]) Has(key K) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.Store[key]
	return ok
}

func (c *CacheMap[K, V]) Length() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.Store)
}

// Delete removes a key from the cache
func (c *CacheMap[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.Store, key)
}

// Load replaces the Store with a new map
func (c *CacheMap[K, V]) Load(m map[K]V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.Store = m
}

// Do runs a custom function with exclusive access to the cache
func (c *CacheMap[K, V]) Do(fn func(c *CacheMap[K, V])) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

// Do executes a function on the value for a given key if it exists
func (c *CacheMap[K, V]) DoMut(key K, fn func(value V)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if val, ok := c.Store[key]; ok {
		fn(val)
	}
}

// SaveToFile persists the cache map to disk using gob
func (c *CacheMap[K, V]) SaveToFile(filename string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	enc := gob.NewEncoder(file)
	return enc.Encode(c.Store)
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
