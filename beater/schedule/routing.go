// Package schedule holds the scheduler event model, the bus routing windows built from it and
// the store that the beater uses to look up the scheduled primary and backup for a bus
package schedule

import (
	"slices"
	"time"
)

// DateLayout is the scheduler start_date / end_date format
const DateLayout = "2006/01/02 15:04:05"

// BusRouting is the scheduled primary and backup source for a bus during a window
type BusRouting struct {
	Pri       string
	Sec       string
	StartDate *time.Time
	EndDate   *time.Time
}

// ParseDate parses a scheduler date string ("2006/01/02 15:04:05")
func ParseDate(s string) (*time.Time, error) {
	t, err := time.Parse(DateLayout, s)
	if err != nil {
		return nil, err
	}

	return &t, nil
}

// IsActive reports whether t is between start and end (inclusive). nil bounds are never active
func IsActive(t time.Time, start, end *time.Time) bool {
	if start == nil || end == nil {
		return false
	}

	return !t.Before(*start) && !t.After(*end)
}

// WindowsOverlap reports whether the start/end windows of two routings overlap (inclusive bounds)
func WindowsOverlap(a, b *BusRouting) bool {
	if !hasWindow(a) || !hasWindow(b) {
		return false
	}

	return !a.StartDate.After(*b.EndDate) && !b.StartDate.After(*a.EndDate)
}

// MergeBusRouting returns a new routing with the incoming routing merged over the existing one.
// if there's no existing routing or the schedule windows don't overlap, the incoming routing replaces it.
// neither argument is modified
func MergeBusRouting(existing, incoming *BusRouting) *BusRouting {
	if existing == nil || !WindowsOverlap(existing, incoming) {
		merged := *incoming
		return &merged
	}

	merged := *existing

	if incoming.Pri != "" {
		merged.Pri = incoming.Pri
	}
	if incoming.Sec != "" {
		merged.Sec = incoming.Sec
	}

	merged.StartDate = incoming.StartDate
	merged.EndDate = incoming.EndDate

	return &merged
}

// hasWindow reports whether the routing has both a start and an end date
func hasWindow(r *BusRouting) bool {
	return r != nil && r.StartDate != nil && r.EndDate != nil
}

// sameWindow reports whether two routings have identical start and end dates
func sameWindow(a, b *BusRouting) bool {
	return hasWindow(a) && hasWindow(b) && a.StartDate.Equal(*b.StartDate) && a.EndDate.Equal(*b.EndDate)
}

// strictlyOverlap reports whether two windows share more than a boundary instant, so back-to-back
// windows (one ends when the next starts) don't overlap
func strictlyOverlap(a, b *BusRouting) bool {
	return hasWindow(a) && hasWindow(b) && a.StartDate.Before(*b.EndDate) && b.StartDate.Before(*a.EndDate)
}

// sortByStart sorts the routings by start date, then end date
func sortByStart(routes []*BusRouting) {
	slices.SortStableFunc(routes, func(a, b *BusRouting) int {
		if c := a.StartDate.Compare(*b.StartDate); c != 0 {
			return c
		}
		return a.EndDate.Compare(*b.EndDate)
	})
}
