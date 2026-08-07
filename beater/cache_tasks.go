package beater

import (
	"context"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	insite "github.com/thetherington/routebeat/beater/analytics"
)

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

	for {
		select {
		case <-done:
			logp.Warn("exiting busCache save to file routine")
			if err := busCache.SaveToFile(BUSCACHE_FILE); err != nil {
				logp.Err("failed to save busCache to file: %v", err)
			}
			return
		case <-ticker.C:
		}

		if err := busCache.SaveToFile(BUSCACHE_FILE); err != nil {
			logp.Err("failed to save busCache to file: %v", err)
		}
	}
}
