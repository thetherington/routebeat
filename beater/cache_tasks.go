package beater

import (
	"context"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	insite "github.com/thetherington/routebeat/beater/analytics"
)

// run in the background to query the analytics schedule index and update the scheduleCache
func AnalyticsQueryGoRoutine(period time.Duration, done <-chan struct{}) {
	ticker := time.NewTicker(period)

	for {
		bm, err := func() (insite.BusRouteMap, error) {
			ctx, cancel := context.WithTimeout(context.Background(), ANALYTICS_TIMEOUT*time.Second)
			defer cancel()

			return db.QuerySchedulerEventParams(ctx)
		}()
		if err != nil || len(bm) == 0 {
			logp.Err("failed QueryScheduler(): %v", err)
		}

		scheduleCache.Load(bm)

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
