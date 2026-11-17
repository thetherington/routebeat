package beater

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/thetherington/routebeat/beater/eventsapi"
	"github.com/thetherington/routebeat/beater/schedule"
)

// counts of the scheduler events received from the events api (poll and refresh), logged every 5 minutes
var scheduleEventCounters struct {
	events   int64
	nonApg   int64
	invalid  int64
	deleted  int64
	noTag    int64
	routings int64
}

// addScheduleEventStats adds parse and build stats to the scheduleEventCounters
func addScheduleEventStats(s schedule.Stats) {
	atomic.AddInt64(&scheduleEventCounters.events, int64(s.Events))
	atomic.AddInt64(&scheduleEventCounters.nonApg, int64(s.NonApg))
	atomic.AddInt64(&scheduleEventCounters.invalid, int64(s.Invalid))
	atomic.AddInt64(&scheduleEventCounters.deleted, int64(s.Deleted))
	atomic.AddInt64(&scheduleEventCounters.noTag, int64(s.NoTag))
	atomic.AddInt64(&scheduleEventCounters.routings, int64(s.Routings))
}

// logScheduleEventCounters periodically logs and resets the scheduleEventCounters
func logScheduleEventCounters(done <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			logp.Debug(
				"ScheduleEvents",
				"events=%d non-apg=%d invalid=%d deleted=%d apg-no-tag=%d routings=%d",
				atomic.SwapInt64(&scheduleEventCounters.events, 0),
				atomic.SwapInt64(&scheduleEventCounters.nonApg, 0),
				atomic.SwapInt64(&scheduleEventCounters.invalid, 0),
				atomic.SwapInt64(&scheduleEventCounters.deleted, 0),
				atomic.SwapInt64(&scheduleEventCounters.noTag, 0),
				atomic.SwapInt64(&scheduleEventCounters.routings, 0),
			)
		}
	}
}

// relativeTime formats a duration as an api relative time offset ("now+25m", "now+1h")
func relativeTime(d time.Duration) string {
	switch {
	case d <= 0:
		return "now"
	case d%time.Hour == 0:
		return fmt.Sprintf("now+%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("now+%dm", d/time.Minute)
	default:
		return fmt.Sprintf("now+%ds", d/time.Second)
	}
}

// eventsAt fetches the events active at a relative time ("now", "now+25m")
type eventsAt func(ctx context.Context, at string) ([]schedule.Event, schedule.Stats, error)

// scheduleFetch is the result of fetching the events active now and at now+lookahead
type scheduleFetch struct {
	routes      map[string][]*schedule.BusRouting // merged now and lookahead windows
	ahead       string                            // the lookahead relative time, e.g. "now+25m"
	nowEvents   int
	aheadEvents int
	aheadErr    error // the lookahead fetch failed, routes only hold the now windows
}

// fetchScheduleAt fetches the events active at the relative time and builds the routing windows
func fetchScheduleAt(fetch eventsAt, at string) (map[string][]*schedule.BusRouting, schedule.Stats, error) {
	events, stats, err := fetch(context.Background(), at)
	if err != nil {
		return nil, stats, err
	}

	routes, buildStats := schedule.BuildRoutes(events)
	stats.Add(buildStats)

	return routes, stats, nil
}

// fetchSchedule fetches the events active now, then at now+lookahead, and merges the routing
// windows. used by the poll (every output) and the recheck (one output) so both build the schedule
// the same way. returns an error only when the "now" fetch fails, the lookahead request is then
// not sent. a failed lookahead fetch is returned in aheadErr with the now windows still built
func fetchSchedule(fetch eventsAt, lookahead time.Duration) (scheduleFetch, error) {
	res := scheduleFetch{ahead: relativeTime(lookahead)}

	current, nowStats, err := fetchScheduleAt(fetch, "now")
	addScheduleEventStats(nowStats)

	if err != nil {
		return res, err
	}

	upcoming, aheadStats, err := fetchScheduleAt(fetch, res.ahead)
	addScheduleEventStats(aheadStats)

	res.routes = schedule.MergeRouteLists(current, upcoming)
	res.nowEvents = nowStats.Events
	res.aheadEvents = aheadStats.Events
	res.aheadErr = err

	return res, nil
}

// pollSchedule fetches the events active now and at now+lookahead and replaces the schedule store.
// the store is left as-is when the "now" fetch fails, so a failed poll never empties the schedule
func pollSchedule(api *eventsapi.Client, lookahead time.Duration) {
	fetchStart := time.Now()

	res, err := fetchSchedule(api.EventsAt, lookahead)
	if err != nil {
		logp.Err("schedule poll failed at=now: %v, keeping previous schedule", err)
		return
	}

	if res.aheadErr != nil {
		logp.Err("schedule poll failed at=%s: %v", res.ahead, res.aheadErr)
	}

	kept := scheduleStore.Replace(res.routes, fetchStart)

	logp.Debug("QueryScheduler", "schedule poll now=%d %s=%d events, outputs=%d kept=%d",
		res.nowEvents, res.ahead, res.aheadEvents, scheduleStore.Length(), kept)
}

// ScheduleQueryGoRoutine polls the events api every period to rebuild the schedule store
func ScheduleQueryGoRoutine(api *eventsapi.Client, period, lookahead time.Duration, done <-chan struct{}) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	for {
		pollSchedule(api, lookahead)

		select {
		case <-done:
			logp.Warn("exiting schedule poll routine")
			return
		case <-ticker.C:
		}
	}
}

// refreshOutput runs the poll for one output: the events active now and at now+lookahead, with the
// output filter, replace the output's routing windows in the schedule store. events for other
// outputs in the responses are ignored, the poll keeps those current. returns whether the store was
// updated: a failed "now" fetch, or no current or upcoming window for the output, keeps the polled
// windows. a failed lookahead fetch is logged and the now windows are still applied, as in the poll
func (bt *routebeat) refreshOutput(output string) (bool, error) {
	fetch := func(ctx context.Context, at string) ([]schedule.Event, schedule.Stats, error) {
		return bt.eventsAPI.OutputEventsAt(ctx, output, at)
	}

	res, err := fetchSchedule(fetch, bt.config.Schedule.Lookahead)
	if err != nil {
		return false, err
	}

	if res.aheadErr != nil {
		logp.Warn("events api refresh failed for Destination:%s at=%s: %v", output, res.ahead, res.aheadErr)
	}

	return scheduleStore.ReplaceOutput(output, res.routes[output], time.Now()), nil
}

// run in the background to save the busCache and the schedule store to file
func SaveCachesGoRoutine(done <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			logp.Warn("exiting cache save to file routine")
			saveCaches()

			return
		case <-ticker.C:
		}

		saveCaches()
	}
}

// saveCaches saves the busCache and the schedule store to their files
func saveCaches() {
	if err := busCache.SaveToFile(BUSCACHE_FILE); err != nil {
		logp.Err("failed to save busCache to file: %v", err)
	}

	// drop expired windows first so only current and upcoming routings are saved
	scheduleStore.Prune(time.Now())

	if err := scheduleStore.SaveToFile(SCHEDULE_FILE); err != nil {
		logp.Err("failed to save schedule store to file: %v", err)
	}
}

// PruneScheduleRoutine removes expired windows from the schedule store every SCHEDULE_PRUNE_PERIOD
func PruneScheduleRoutine(done <-chan struct{}) {
	ticker := time.NewTicker(SCHEDULE_PRUNE_PERIOD)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			windows, outputs := scheduleStore.Prune(time.Now())
			logp.Debug("PruneSchedule", "removed %d expired windows, %d empty outputs", windows, outputs)
		case <-done:
			logp.Warn("exiting schedule prune routine")
			return
		}
	}
}

// GetBusRouting retrieves the bus routing for a destination from the schedule store.
// returns SourceNone when the destination has no (active) routing
func GetBusRouting(destination string, evalEndDate bool) (*schedule.BusRouting, RoutingSource, bool) {
	routing, ok := scheduleStore.Active(destination, time.Now(), evalEndDate)
	if !ok {
		return nil, SourceNone, false
	}

	return routing, SourceCache, true
}
