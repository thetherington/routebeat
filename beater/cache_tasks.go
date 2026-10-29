package beater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	insite "github.com/thetherington/routebeat/beater/analytics"
	"github.com/thetherington/routebeat/beater/cache"
)

var schedulerEventCounters struct {
	nonApg     int64
	invalid    int64
	noTag      int64
	apgInTime  int64
	apgOutTime int64
}

// quoted task value used to cheaply discard non-apg events before any json decoding
var apgTaskBytes = []byte(`"apg"`)

// logSchedulerEventCounters periodically logs the counts of different scheduler events.
func logSchedulerEventCounters(done <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			nonApg := atomic.SwapInt64(&schedulerEventCounters.nonApg, 0)
			invalid := atomic.SwapInt64(&schedulerEventCounters.invalid, 0)
			noTag := atomic.SwapInt64(&schedulerEventCounters.noTag, 0)
			apgInTime := atomic.SwapInt64(&schedulerEventCounters.apgInTime, 0)
			apgOutTime := atomic.SwapInt64(&schedulerEventCounters.apgOutTime, 0)

			logp.Debug(
				"SchedulerHookCallback",
				"non-apg=%d invalid=%d apg-no-tag=%d apg-in-active=%d apg-outside-active=%d",
				nonApg,
				invalid,
				noTag,
				apgInTime,
				apgOutTime,
			)
		}
	}
}

func querySchedule(logLabel string, opt ...string) insite.BusRouteMap {
	ctx, cancel := context.WithTimeout(context.Background(), ANALYTICS_TIMEOUT*time.Second)
	defer cancel()

	bm, err := db.QuerySchedulerEventParams(ctx, opt...)
	if err != nil || len(bm) == 0 {
		logp.Err("failed %s: %v", logLabel, err)
	}

	return bm
}

func mergeBusRouteMaps(now, ahead insite.BusRouteMap) map[string][]*insite.BusRouting {
	merged := make(map[string][]*insite.BusRouting, len(now)+len(ahead))

	for key, route := range now {
		merged[key] = []*insite.BusRouting{route}
	}

	for key, route := range ahead {
		if routes, ok := merged[key]; ok {
			merged[key] = append(routes, route)
			continue
		}

		// if the key doesn't exist in the merged map, create a new slice with the route
		merged[key] = []*insite.BusRouting{nil, route}
	}

	return merged
}

// run in the background to query the analytics schedule index and update the scheduleCache
func AnalyticsQueryGoRoutine(period time.Duration, done <-chan struct{}) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	for {
		bm_now := querySchedule("QueryScheduler()")
		bm_ahead := querySchedule("QueryScheduler(now+25m)", "now+25m")

		scheduleCache.Load(mergeBusRouteMaps(bm_now, bm_ahead))

		logp.Debug("QueryScheduler", "cache updated with %d keys", scheduleCache.Length())

		select {
		case <-done:
			logp.Warn("exiting elasticsearch QueryScheduler() routine")
			return
		case <-ticker.C:
		}
	}
}

// run in the background to save the buscache to a file
func SaveBusCacheGoRoutine(done <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			logp.Warn("exiting busCache save to file routine")

			if err := busCache.SaveToFile(BUSCACHE_FILE); err != nil {
				logp.Err("failed to save busCache to file: %v", err)
			}
			// if err := scheduleHotCache.SaveToFile(HOTCACHE_FILE); err != nil {
			// 	logp.Err("failed to save scheduleHotCache to file: %v", err)
			// }

			return
		case <-ticker.C:
		}

		if err := busCache.SaveToFile(BUSCACHE_FILE); err != nil {
			logp.Err("failed to save busCache to file: %v", err)
		}
		// if err := scheduleHotCache.SaveToFile(HOTCACHE_FILE); err != nil {
		// 	logp.Err("failed to save scheduleHotCache to file: %v", err)
		// }
	}
}

// SchedulerHookCallback parses a scheduler payload (single event object or array of events)
// and updates the scheduleHotCache with any active apg bus routing. Used by both the
// webhook server and the on-demand events api client.
//
// returns an error only when the payload is not valid JSON. valid JSON that holds no apg events,
// or apg events that are not active, is silently skipped
func SchedulerHookCallback(ctx context.Context, payload []byte) error {
	events, err := parseSchedulerEvents(payload)
	if err != nil {
		return err
	}

	if len(events) == 0 {
		return nil
	}

	updated := applySchedulerEvents(events, time.Now())

	logp.Debug("SchedulerHookCallback", "received %d apg events, updated %d hot cache outputs", len(events), updated)

	return nil
}

// parseSchedulerEvents decodes a payload into apg events. Items that are not apg or fail to
// decode are counted and skipped. Only an unparsable payload returns an error
func parseSchedulerEvents(payload []byte) ([]SchedulerAPGEvent, error) {
	var items []json.RawMessage

	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty payload after trimming")
	}

	switch trimmed[0] {
	case '{':
		if !json.Valid(trimmed) {
			return nil, fmt.Errorf("malformed JSON object payload")
		}

		items = []json.RawMessage{trimmed}

	case '[':
		// unmarshal validates the whole array
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("malformed JSON array payload: %v", err)
		}

	default:
		if !json.Valid(trimmed) {
			return nil, fmt.Errorf("payload is not JSON")
		}

		// valid JSON that is not an event (string, number, boolean or null)
		atomic.AddInt64(&schedulerEventCounters.nonApg, 1)
		return nil, nil
	}

	events := make([]SchedulerAPGEvent, 0, len(items))

	for _, item := range items {
		// cheap pre-filter: discard items without an "apg" value before any json decoding
		if !bytes.Contains(item, apgTaskBytes) {
			atomic.AddInt64(&schedulerEventCounters.nonApg, 1)
			continue
		}

		// decode only the task field to confirm the task type. other event shapes may not fit SchedulerAPGEvent
		var task schedulerTask
		if err := json.Unmarshal(item, &task); err != nil {
			atomic.AddInt64(&schedulerEventCounters.invalid, 1)
			continue
		}

		if task.Task != "apg" {
			atomic.AddInt64(&schedulerEventCounters.nonApg, 1)
			continue
		}

		var event SchedulerAPGEvent
		if err := json.Unmarshal(item, &event); err != nil {
			atomic.AddInt64(&schedulerEventCounters.invalid, 1)
			logp.Debug("SchedulerHookCallback", "failed to unmarshal apg event: %v", err)
			continue
		}

		events = append(events, event)
	}

	return events, nil
}

// applySchedulerEvents converts the events into bus routings active at now and merges them
// into the scheduleHotCache. returns the number of outputs updated
func applySchedulerEvents(events []SchedulerAPGEvent, now time.Time) int {
	// holder for the bus routing map to fold multiple events for the same output
	busRoutingMap := make(map[string]*insite.BusRouting)

	for _, event := range events {
		br, err := event.busRouting(now)
		if err != nil {
			switch {
			case errors.Is(err, errEventInactive):
				atomic.AddInt64(&schedulerEventCounters.apgOutTime, 1)
			case errors.Is(err, errEventNoTag):
				atomic.AddInt64(&schedulerEventCounters.noTag, 1)
			default:
				atomic.AddInt64(&schedulerEventCounters.invalid, 1)
				logp.Err("skipping scheduler event %s: %v", event.Output, err)
			}

			continue
		}

		atomic.AddInt64(&schedulerEventCounters.apgInTime, 1)

		logp.Debug("SchedulerHookCallback", "event %s is active now", event.Output)

		busRoutingMap[event.Output] = mergeBusRouting(busRoutingMap[event.Output], br)
	}

	if len(busRoutingMap) == 0 {
		return 0
	}

	// merge into the hot cache with exclusive access. values are replaced, never mutated in place,
	// so readers holding a previous routing pointer are not affected
	scheduleHotCache.Do(func(c *cache.CacheMap[string, *insite.BusRouting]) {
		for output, routing := range busRoutingMap {
			logp.Debug("SchedulerHookCallback", "updating hot cache for output: %s", output)

			c.Store[output] = mergeBusRouting(c.Store[output], routing)
		}
	})

	return len(busRoutingMap)
}

// mergeBusRouting returns a new routing with the incoming routing merged over the existing one.
// if there's no existing routing or the schedule windows don't overlap, the incoming routing replaces it.
// neither argument is modified
func mergeBusRouting(existing, incoming *insite.BusRouting) *insite.BusRouting {
	if existing == nil || !windowsOverlap(existing, incoming) {
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

// windowsOverlap reports whether the start/end windows of two routings overlap
func windowsOverlap(a, b *insite.BusRouting) bool {
	if a.StartDate == nil || a.EndDate == nil || b.StartDate == nil || b.EndDate == nil {
		return false
	}

	return !a.StartDate.After(*b.EndDate) && !b.StartDate.After(*a.EndDate)
}

// isActive reports whether t is between start and end (inclusive). nil bounds are never active
func isActive(t time.Time, start, end *time.Time) bool {
	if start == nil || end == nil {
		return false
	}

	return !t.Before(*start) && !t.After(*end)
}

// cleanup the hot cache by removing any entries that are no longer active based on the current time every HOTCACHE_CLEANUP_PERIOD
func CleanupScheduleHotCache(done <-chan struct{}) {
	ticker := time.NewTicker(HOTCACHE_CLEANUP_PERIOD)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if removed := PruneScheduleHotCache(time.Now()); removed > 0 {
				logp.Debug("CleanupScheduleHotCache", "removed %d expired hot cache entries", removed)
			}
		case <-done:
			logp.Warn("exiting hot cache cleanup routine")
			return
		}
	}
}

// PruneScheduleHotCache removes hot cache entries that have expired at now, or have no start/end date
// and can never be active. returns the number of entries removed
func PruneScheduleHotCache(now time.Time) int {
	var removed int

	// delete under the same lock as the scan so a fresh entry written in between is never removed
	scheduleHotCache.Do(func(c *cache.CacheMap[string, *insite.BusRouting]) {
		for key, routing := range c.Store {
			if routing == nil || routing.StartDate == nil || routing.EndDate == nil || now.After(*routing.EndDate) {
				logp.Debug("CleanupScheduleHotCache", "removing expired hot cache entry for output: %s", key)
				delete(c.Store, key)
				removed++
			}
		}
	})

	return removed
}

// GetHotRoute retrieves the bus routing for a given destination from the hot cache if it is active based on the current time.
func GetScheduleHotRoute(destination string, t time.Time) (*insite.BusRouting, bool) {
	routing, ok := scheduleHotCache.Get(destination)
	if !ok || routing == nil || !isActive(t, routing.StartDate, routing.EndDate) {
		return nil, false
	}

	return routing, true
}

// GetBusRoutingFromCaches retrieves the bus routing for a given destination from the hot cache or the main cache.
// the hot cache is only consulted when useHot is true. returns which cache supplied the routing
func GetBusRoutingFromCaches(destination string, evalEndDate, useHot bool) (*insite.BusRouting, RoutingSource, bool) {
	var routing *insite.BusRouting

	now := time.Now()

	// check if there's a hot route, the hot route takes precedence over the main cache
	if useHot {
		if r, ok := GetScheduleHotRoute(destination, now); ok {
			return r, SourceHotCache, true
		}
	}

	routings, _ := scheduleCache.Get(destination)

	// select the routing based if where now is between the routing.StartDate and routing.EndDate
	for _, r := range routings {
		if r == nil {
			continue
		}

		// if we are not validating end dates just take the first routing in the list and return it
		if !evalEndDate || isActive(now, r.StartDate, r.EndDate) {
			routing = r
			break
		}
	}

	if routing == nil {
		return nil, SourceNone, false
	}

	return routing, SourceCache, true
}
