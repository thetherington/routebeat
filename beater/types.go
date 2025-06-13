package beater

import (
	"encoding/gob"
	"os"
	"sync"
)

// EventType is a small enum
type EventType int

const (
	Query EventType = iota
	Notification
)

var eventName = map[EventType]string{
	Query:        "query",
	Notification: "notification",
}

func (et EventType) String() string {
	return eventName[et]
}

// RoutingState is a small enum
type RoutingState int

const (
	Primary RoutingState = iota
	Backup
	Zorro
	TDA
)

var routingName = map[RoutingState]string{
	Primary: "Primary",
	Backup:  "Backup",
	Zorro:   "Zorro",
	TDA:     "TDA",
}

func (rs RoutingState) String() string {
	return routingName[rs]
}

// generic, thread-safe cache
type CacheMap[K comparable, V any] struct {
	mu    sync.RWMutex
	store map[K]V
}

// NewCacheMap initializes and returns a new cache
func NewCacheMap[K comparable, V any]() *CacheMap[K, V] {
	return &CacheMap[K, V]{
		store: make(map[K]V),
	}
}

// Get retrieves a value from the cache and a boolean if found
func (c *CacheMap[K, V]) Get(key K) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	val, ok := c.store[key]

	return val, ok
}

// Set adds or updates a value in the cache
func (c *CacheMap[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.store[key] = value
}

// Delete removes a key from the cache
func (c *CacheMap[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.store, key)
}

// Load replaces the store with a new map
func (c *CacheMap[K, V]) Load(m map[K]V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.store = m
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
	return enc.Encode(c.store)
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
	return dec.Decode(&c.store)
}
