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

// ReplaceOutput replaces the windows of one output with a refreshed snapshot (the windows built from
// every event the api returned for the output), the same way a full poll replaces every output.
// windows that ended before at, or have no window, are dropped. when no window is left the stored
// entry is kept as-is, so an empty or failed refresh never removes the polled schedule.
// returns false when the entry was kept
func (s *Store) ReplaceOutput(output string, windows []*BusRouting, at time.Time) bool {
	routes := make([]*BusRouting, 0, len(windows))

	for _, w := range windows {
		if !hasWindow(w) || w.EndDate.Before(at) {
			continue
		}

		c := *w
		routes = append(routes, &c)
	}

	if len(routes) == 0 {
		return false
	}

	sortByStart(routes)

	s.c.Do(func(c *cache.CacheMap[string, *Entry]) {
		c.Store[output] = &Entry{Routes: routes, UpdatedAt: at}
	})

	return true
}

// Active returns the routing for the output that is active at t. when several windows are active
// (a shorter event inside a longer one, or back-to-back windows at the boundary instant) the one
// whose start is closest to t wins, which is the latest start since every active window has
// started. windows with the same start resolve to the shorter one. when evalEndDate is false the
// first stored routing is returned whatever its window
func (s *Store) Active(output string, t time.Time, evalEndDate bool) (*BusRouting, bool) {
	entry, ok := s.c.Get(output)
	if !ok || entry == nil {
		return nil, false
	}

	var best *BusRouting

	for _, r := range entry.Routes {
		if r == nil {
			continue
		}

		if !evalEndDate {
			return r, true
		}

		if !IsActive(t, r.StartDate, r.EndDate) {
			continue
		}

		if best == nil || r.StartDate.After(*best.StartDate) ||
			(r.StartDate.Equal(*best.StartDate) && r.EndDate.Before(*best.EndDate)) {
			best = r
		}
	}

	return best, best != nil
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
