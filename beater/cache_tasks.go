package beater

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	insite "github.com/thetherington/routebeat/beater/analytics"
	"github.com/thetherington/routebeat/beater/cache"
)

var schedulerEventCounters struct {
	nonApg     int64
	apgInTime  int64
	apgOutTime int64
}

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
			apgInTime := atomic.SwapInt64(&schedulerEventCounters.apgInTime, 0)
			apgOutTime := atomic.SwapInt64(&schedulerEventCounters.apgOutTime, 0)

			logp.Debug(
				"SchedulerHookCallback",
				"non-apg=%d apg-in-active=%d apg-outside-active=%d",
				nonApg,
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

// run in the background to save the scheduleCache to a file
func SchedulerHookCallback(ctx context.Context, payload []byte) error {
	var events []SchedulerAPGEvent

	type TaskField struct {
		Task string `json:"task"`
	}

	switch jsonType := checkJSONType(payload); jsonType {
	case "empty":
		return fmt.Errorf("empty payload after trimming")

	case "other (string, number, boolean, or invalid)":
		return fmt.Errorf("invalid JSON payload type: %s", jsonType)

	case "object":
		// unmarshal the payload into a TaskField to check the task type
		var tf TaskField
		if err := json.Unmarshal(payload, &tf); err != nil {
			return fmt.Errorf("failed to unmarshal object payload: %v", err)
		}

		// return without error if the task is not "apg"
		if tf.Task != "apg" {
			atomic.AddInt64(&schedulerEventCounters.nonApg, 1)
			return nil
		}

		// if the task is "apg", proceed to unmarshal the payload into the events slice
		var event SchedulerAPGEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return fmt.Errorf("failed to unmarshal object payload into SchedulerAPGEvent: %v", err)
		}

		events = append(events, event)

	case "array":
		// handle array of events
		var arr []json.RawMessage
		if err := json.Unmarshal(payload, &arr); err != nil {
			return fmt.Errorf("failed to unmarshal array payload: %v", err)
		}

		for _, item := range arr {
			// unmarshal the array element into a TaskField to check the task type before processing
			var tf TaskField
			if err := json.Unmarshal(item, &tf); err != nil {
				return fmt.Errorf("failed to unmarshal array element into TaskField: %v", err)
			}

			// skip the array element if the task is not "apg"
			if tf.Task != "apg" {
				atomic.AddInt64(&schedulerEventCounters.nonApg, 1)
				continue
			}

			// unmarshal the array element into a SchedulerAPGEvent since the task is "apg"
			var event SchedulerAPGEvent
			if err := json.Unmarshal(item, &event); err != nil {
				return fmt.Errorf("failed to unmarshal array element into SchedulerAPGEvent: %v", err)
			}

			events = append(events, event)
		}

		if len(events) == 0 {
			return nil
		}
	}

	// execute the cache update in a fire and forget go routine and return nil
	go func() {
		// holder for the bus routing map to update the hot cache incase of multiple events for different outputs
		busRoutingMap := make(map[string]*insite.BusRouting)

		// iterate over each payload event, parse the start and end date and create bus routing objects to update the hot cache
		for _, event := range events {
			if event.Task != "apg" {
				logp.Debug("SchedulerHookCallback", "skipping event %s with task %s", event.Output, event.Task)
				continue
			}

			// parse the start and end date
			startTime, err := time.Parse("2006/01/02 15:04:05", event.StartDate)
			if err != nil {
				logp.Err("failed to parse start date for event %s: %v", event.Output, err)
				continue
			}

			endTime, err := time.Parse("2006/01/02 15:04:05", event.EndDate)
			if err != nil {
				logp.Err("failed to parse end date for event %s: %v", event.Output, err)
				continue
			}

			now := time.Now()
			isBetween := (now.After(startTime) || now.Equal(startTime)) && (now.Before(endTime) || now.Equal(endTime))

			// check if the event is active now, if not skip it
			// an event is active if the current time is between the start and end time
			if !isBetween {
				atomic.AddInt64(&schedulerEventCounters.apgOutTime, 1)
				continue
			}

			atomic.AddInt64(&schedulerEventCounters.apgInTime, 1)

			logp.Debug("SchedulerHookCallback", "event %s is active now", event.Output)

			// create a bus routing object
			br := &insite.BusRouting{
				StartDate: &startTime,
				EndDate:   &endTime,
			}

			// determine if the event is a primary or secondary bus routing based on the tags
			if slices.Contains(event.Tags, "MAIN") {
				br.Pri = event.Input
			} else if slices.Contains(event.Tags, "BACKUP") {
				br.Sec = event.Input
			}

			// update the bus routing map with the new bus routing
			routing, ok := busRoutingMap[event.Output]
			if !ok {
				busRoutingMap[event.Output] = br
				continue
			}

			// if the routing already exists, update the existing routing with the new bus routing
			if br.Pri != "" {
				routing.Pri = br.Pri
			}
			if br.Sec != "" {
				routing.Sec = br.Sec
			}

			busRoutingMap[event.Output] = routing
		}

		// check to see if the busRoutingMap contains any entries before updating the hot cache
		if len(busRoutingMap) == 0 {
			return
		}

		// acquire a lock on the scheduleCache to ensure thread-safe updates
		scheduleCache.Lock()
		defer scheduleCache.Unlock()

		// iterate over the bus routing map and update the hot cache
		for output, routing := range busRoutingMap {
			logp.Debug("SchedulerHookCallback", "updating hot cache for output: %s", output)

			// if there's both a primary and secondary routing then replace the cache entirely
			if routing.Pri != "" && routing.Sec != "" {
				scheduleHotCache.Set(output, routing)
				continue
			}

			// of there's only a primary or secondary routing, then get the cache item, check that it exists, and update the event times and routing accordingly
			cachedRouting, ok := scheduleHotCache.Get(output)
			if ok {
				if routing.Pri != "" {
					cachedRouting.Pri = routing.Pri
				}
				if routing.Sec != "" {
					cachedRouting.Sec = routing.Sec
				}
				if routing.StartDate != nil {
					cachedRouting.StartDate = routing.StartDate
				}
				if routing.EndDate != nil {
					cachedRouting.EndDate = routing.EndDate
				}

				scheduleHotCache.Set(output, cachedRouting)
			} else {
				// if the cache item doesn't exist, add the new routing to the cache
				scheduleHotCache.Set(output, routing)
			}
		}
	}()

	return nil
}

// cleanup the hot cache by removing any entries that are no longer active based on the current time every 24 hours
func CleanupScheduleHotCache(done <-chan struct{}) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()

			keysToDelete := make([]string, 0)

			scheduleHotCache.Do(func(c *cache.CacheMap[string, *insite.BusRouting]) {
				for key, routing := range c.Store {
					if routing.EndDate != nil && now.After(*routing.EndDate) {
						keysToDelete = append(keysToDelete, key)
					}
				}
			})

			for _, key := range keysToDelete {
				logp.Debug("CleanupScheduleHotCache", "removing inactive hot cache entry for output: %s", key)
				scheduleHotCache.Delete(key)
			}
		case <-done:
			logp.Warn("exiting hot cache cleanup routine")
			return
		}
	}
}

// GetHotRoute retrieves the bus routing for a given destination from the hot cache if it is active based on the current time.
func GetScheduleHotRoute(destination string, t time.Time) (*insite.BusRouting, bool) {
	routing, ok := scheduleHotCache.Get(destination)
	if !ok || routing.StartDate == nil || routing.EndDate == nil {
		return nil, false
	}

	isBetween := (t.After(*routing.StartDate) || t.Equal(*routing.StartDate)) && (t.Before(*routing.EndDate) || t.Equal(*routing.EndDate))
	if isBetween {
		return routing, true
	}

	return nil, false
}

// GetBusRoutingFromCaches retrieves the bus routing for a given destination from the hot cache or the main cache.
func GetBusRoutingFromCaches(destination string, evalEndDate bool) (*insite.BusRouting, bool) {
	var routing *insite.BusRouting

	now := time.Now()

	// first check the main cache for the routing
	routings, _ := scheduleCache.Get(destination)

	// select the routing based if where now is between the routing.StartDate and routing.EndDate
	for _, r := range routings {
		if r == nil {
			continue
		}

		// if we are not validating end dates just take the first routing in the list and return it
		if !evalEndDate {
			routing = r
			return routing, true
		}

		if r.StartDate == nil || r.EndDate == nil {
			continue
		}
		if (now.After(*r.StartDate) || now.Equal(*r.StartDate)) && (now.Before(*r.EndDate) || now.Equal(*r.EndDate)) {
			routing = r
			break
		}
	}

	// check if there's a hot route, if no hot route, return the routing from the cache, otherwise return the hot route
	if r, ok := GetScheduleHotRoute(destination, now); ok {
		return r, true
	}

	return routing, routing != nil
}
