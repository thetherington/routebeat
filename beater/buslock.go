package beater

import "sync"

// busLocks serializes the processing of each destination: the routing lookup, reading the
// stored state, the recheck, and writing the new state run as one step per bus.
// notifications and queries for other buses are not blocked
var busLocks keyedMutex

// keyedMutex holds one mutex per key, created on first use. mutexes are never removed,
// there's one per destination (the same as the busCache entries)
type keyedMutex struct{ m sync.Map } // string -> *sync.Mutex

// Lock locks the key's mutex and returns its unlock function
func (k *keyedMutex) Lock(key string) (unlock func()) {
	v, _ := k.m.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()

	return mu.Unlock
}
