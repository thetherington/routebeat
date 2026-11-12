package schedule

import (
	"slices"
	"time"

	"github.com/thetherington/routebeat/beater/cache"
)

// Entry is the stored routing windows for one output, sorted by start date
type Entry struct {
	Routes    []*BusRouting
	UpdatedAt time.Time // when the windows were fetched (poll start) or refreshed
}

// Store is the thread-safe schedule keyed by output (bus code). Entries and routings are replaced,
// never modified in place, so a reader holding a routing pointer is never affected by a write
type Store struct {
	c *cache.CacheMap[string, *Entry]
}

// NewStore creates an empty schedule store
func NewStore() *Store {
	return &Store{c: cache.NewCacheMap[string, *Entry]()}
}

// Replace loads the result of a full poll that began at fetchStart. an existing entry refreshed after
// fetchStart is newer than the poll and is kept. every other entry is replaced or, when the output is
// not in the poll, removed. returns the number of entries kept by that guard
func (s *Store) Replace(routes map[string][]*BusRouting, fetchStart time.Time) int {
	next := make(map[string]*Entry, len(routes))

	for output, list := range routes {
		if len(list) == 0 {
			continue
		}

		next[output] = &Entry{Routes: list, UpdatedAt: fetchStart}
	}

	var kept int

	s.c.Do(func(c *cache.CacheMap[string, *Entry]) {
		for output, entry := range c.Store {
			if entry != nil && entry.UpdatedAt.After(fetchStart) {
				next[output] = entry
				kept++
			}
		}

		c.Store = next
	})

	return kept
}

// Upsert merges refreshed windows for an output, dropping windows that ended before at.
// a window identical to, or strictly overlapping, a stored window is merged over it (an advance or
// fast-forward changes the window, a MAIN or BACKUP only update keeps the other source); any other
// window is inserted. returns false when no window was applied
func (s *Store) Upsert(output string, windows []*BusRouting, at time.Time) bool {
	var applied bool

	s.c.Do(func(c *cache.CacheMap[string, *Entry]) {
		var routes []*BusRouting
		if existing := c.Store[output]; existing != nil {
			routes = slices.Clone(existing.Routes)
		}

		for _, w := range windows {
			if !hasWindow(w) || w.EndDate.Before(at) {
				continue
			}

			routes = mergeWindow(routes, w)
			applied = true
		}

		if !applied {
			return
		}

		sortByStart(routes)
		c.Store[output] = &Entry{Routes: routes, UpdatedAt: at}
	})

	return applied
}

// mergeWindow merges w into the first identical, then strictly overlapping, routing or appends a copy
func mergeWindow(routes []*BusRouting, w *BusRouting) []*BusRouting {
	for _, match := range []func(a, b *BusRouting) bool{sameWindow, strictlyOverlap} {
		for i, r := range routes {
			if match(r, w) {
				routes[i] = MergeBusRouting(r, w)
				return routes
			}
		}
	}

	c := *w

	return append(routes, &c)
}

// Active returns the first routing for the output that is active at t. when evalEndDate is false
// the first stored routing is returned whatever its window
func (s *Store) Active(output string, t time.Time, evalEndDate bool) (*BusRouting, bool) {
	entry, ok := s.c.Get(output)
	if !ok || entry == nil {
		return nil, false
	}

	for _, r := range entry.Routes {
		if r == nil {
			continue
		}

		if !evalEndDate || IsActive(t, r.StartDate, r.EndDate) {
			return r, true
		}
	}

	return nil, false
}

// Routes returns a copy of the stored routing list for the output
func (s *Store) Routes(output string) []*BusRouting {
	entry, ok := s.c.Get(output)
	if !ok || entry == nil {
		return nil
	}

	return slices.Clone(entry.Routes)
}

// Prune removes the windows that ended before now (or have no window) and the outputs left without
// any window. returns the number of windows and outputs removed
func (s *Store) Prune(now time.Time) (windows, outputs int) {
	s.c.Do(func(c *cache.CacheMap[string, *Entry]) {
		for output, entry := range c.Store {
			if entry == nil {
				delete(c.Store, output)
				outputs++
				continue
			}

			keep := make([]*BusRouting, 0, len(entry.Routes))
			for _, r := range entry.Routes {
				if hasWindow(r) && !now.After(*r.EndDate) {
					keep = append(keep, r)
				}
			}

			removed := len(entry.Routes) - len(keep)
			if removed == 0 {
				continue
			}

			windows += removed

			if len(keep) == 0 {
				delete(c.Store, output)
				outputs++
				continue
			}

			c.Store[output] = &Entry{Routes: keep, UpdatedAt: entry.UpdatedAt}
		}
	})

	return windows, outputs
}

// Length returns the number of outputs in the store
func (s *Store) Length() int {
	return s.c.Length()
}

// Clear removes every entry
func (s *Store) Clear() {
	s.c.Load(make(map[string]*Entry))
}

// SaveToFile saves the store to a gob file
func (s *Store) SaveToFile(filename string) error {
	return s.c.SaveToFile(filename)
}

// LoadFromFile replaces the store with the contents of a gob file
func (s *Store) LoadFromFile(filename string) error {
	return s.c.LoadFromFile(filename)
}
