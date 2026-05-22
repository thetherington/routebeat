package beater

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/thetherington/routebeat/beater/analytics"
)

type MagnumLogsOptions struct {
	src       string
	dst       string
	sweeping  bool
	window    time.Duration
	eventTime time.Time
}

type MagnumLogsResponse struct {
	Request  analytics.Source
	Complete analytics.Source
	Swept    bool
}

// ProcessMagnumLogs processes the magnum logs for a given source and destination and returns the request and complete
// logs along with a boolean indicating if sweeping was used to find the logs.
func ProcessMagnumLogs(args *MagnumLogsOptions) (*MagnumLogsResponse, error) {
	var swept bool = false

	logs, err := db.SearchMagnumLogs(args.src, args.dst)
	if err != nil {
		if err == analytics.ErrNoResults {
			return nil, ErrNoLogsFound
		}

		return nil, fmt.Errorf("db.SearchMagnumLogs error: %s", err.Error())
	}

	var (
		request  analytics.Source
		complete analytics.Source
	)

	// check if any of the logs contain the route subscribe request
	for _, log := range logs {
		if strings.Contains(log.Log.Syslog.Message, analytics.REQUEST_LOGS) {
			// compare the timestamp of the request log to the event timestamp to ensure it's within a 10 second window
			if (absDuration(args.eventTime.Sub(log.Device.Timestamp)) <= args.window) && !logCache.Has(log.Id) {
				request = log
				break
			}
		}
	}

	// No request log was found with the configured window and if sweeping is enabled,
	// then we can look for a request log that is outside the window but within the last 20 minutes and use that as the request log.
	// This is to handle cases where this is a non route notification and there are syslog message with no matched notifications.
	if request.Device.Timestamp.IsZero() && args.sweeping {
		for _, log := range logs {
			if strings.Contains(log.Log.Syslog.Message, analytics.REQUEST_LOGS) {
				if !logCache.Has(log.Id) {
					request = log
					swept = true
					break
				}
			}
		}
	}

	if request.Device.Timestamp.IsZero() {
		return nil, ErrNoRequestLogs
	}

	// check if any of the logs contain the route subscribe complete
	for i, log := range logs {
		if strings.Contains(log.Log.Syslog.Message, analytics.COMPLETION_LOGS) {
			// compare the timestamp of the complete log that it's greater than the request time and the log is not in cache.
			if log.Device.Timestamp.After(request.Device.Timestamp) && !logCache.Has(log.Id) {
				// check that if the previous log is a request log and is the matched request log
				// ensures that a request log isn't orphaned without a completion log. If so, then mark the request log id in the cache to prevent future matches and return no completed logs error to prevent publishing an event with just a request log and no completion log.
				if i > 0 && strings.Contains(logs[i-1].Log.Syslog.Message, analytics.REQUEST_LOGS) && logs[i-1].Id != request.Id {
					logp.Err("Orphaned request log found for log id: %v (previous id: %v), source: %s, destination: %s, swept: %v", request.Id, logs[i-1].Id, args.src, args.dst, swept)
					logp.Err("Orphaned log data Previous Log: (%s) Active Request: (%s)", logs[i-1].Log.Syslog.Message, request.Log.Syslog.Message)

					// logCache.Add(request.Id)
					// request = logs[i-1]
				}

				complete = log
				break
			}
		}
	}

	if complete.Device.Timestamp.IsZero() {
		return nil, ErrNoCompletedLogs
	}

	// Mark these ids as processed in the cache
	if request.Id != "" {
		logCache.Set(request.Id, struct{}{})
	}
	if complete.Id != "" {
		// check if the complete log is a salvo log that contains multiple ids of other request logs. if so don't add it to the cache.
		// if the number of occurences of "sub_dst" in the complete log message is greater than 1, then it's a salvo log and we shouldn't add it to the cache
		// because it could be matched to multiple request logs. This is a heuristic that may need to be adjusted based on the actual log messages.
		if strings.Count(complete.Log.Syslog.Message, "sub_dst") <= 1 {
			logCache.Set(complete.Id, struct{}{})
		}
	}

	return &MagnumLogsResponse{
		Request:  request,
		Complete: complete,
		Swept:    swept,
	}, nil
}

// QueryLogsFromEvent collects the system logs for a given event by extracting the busname from the event,
// looking up the physical route information in the cache, and then executing multi-search queries to retrieve the
// logs for each slab in the physical route.
func QueryLogsFromEvent(ctx context.Context, event *beat.Event) (analytics.MultiLogResults, error) {
	// extract srcId and dstId from the event for log analysis
	srcId, dstId, err := getSrcDstIds(event)
	if err != nil {
		return nil, fmt.Errorf("getSrcDstIds error: %s", err.Error())
	}

	// extract the busname from the event to look up the physical route information in the cache
	busname, err := ExtractStringFromEvent(event, "destinationLabel")
	if err != nil {
		return nil, fmt.Errorf("ExtractStringFromEvent error: %s", err.Error())
	}

	// look up the physical route information in the cache using the busname extracted from the event. If there is no busname or no physical route information in the cache for that busname, then we can't collect the slab logs for this event since we won't know which slabs to query for, so return an error.
	slabs, ok := busRoutingCache.Get(busname)
	if !ok {
		return nil, fmt.Errorf("busRoutingCache.Get: no physical route information found in cache for destinationLabel: %s", busname)
	}

	address, err := ExtractStringFromEvent(event, "routeableTerminal.physicalSource.port.address")
	if err != nil {
		return nil, fmt.Errorf("ExtractStringFromEvent error: %s", err.Error())
	}

	mcast, err := ParseMulticastAddress(address)
	if err != nil {
		return nil, fmt.Errorf("ParseMulticastAddress error: %s", err.Error())
	}

	queryOptions := make([]analytics.MultiLogQuery, 0)

	// for each slab in the physical route, create a MultiLogQuery with the source and destination information for that slab to collect the relevant logs for it.
	for _, slab := range slabs {
		q := analytics.NewSlabLogQuery(slab.Device, mcast, slab.Output)
		queryOptions = append(queryOptions, q)
	}

	// also create a MultiLogQuery for the scheduler logs for the overall route from the source to destination to collect relevant logs for the scheduler that may not be tied to a specific slab but could still provide useful information about the route.
	queryOptions = append(queryOptions, analytics.NewSchedulerLogQuery(srcId, dstId))

	// execute the multi-search query to collect the logs for each slab in the physical route as well as the scheduler logs for the overall route. If there are no results found for any of the queries, then return an error since we won't have any logs to analyze for this event.
	logMap, err := db.SearchMultiLogs(ctx, queryOptions...)
	if err != nil {
		return nil, fmt.Errorf("db.SearchMultiLogs error: %s", err.Error())
	}

	return logMap, nil
}

type MatchLogArgs struct {
	logCollection analytics.MultiLogResults
	reference     time.Time
	window        time.Duration
}

// MatchSlabLogs matches the slab logs for a given route completion log by comparing the timestamps of the slab logs
// to the timestamp of the reference log and ensuring that they are within the configured time window and not
// in the cache. It returns a slice of matched slab logs sorted by timestamp in ascending order.
func MatchSlabLogs(args *MatchLogArgs) ([]analytics.Source, error) {
	var matchedLogs []analytics.Source

	// get the keys from args.logCollection that contain "slab"
	for key, logs := range args.logCollection {
		if !strings.Contains(key, "slab") {
			continue
		}

		var found bool
		for _, log := range logs {
			// compare the timestamp of the slab log to the reference log timestamp to ensure
			// it's within the configured window and that it's not in the cache.
			if !log.Device.Timestamp.After(args.reference) {
				continue
			}
			// if the slab log timestamp is greater than the reference log timestamp but outside the
			// configured window, then break out of the loop for this key since the logs are
			// returned in ascending order by timestamp and there won't be any more logs that match for this key.
			if log.Device.Timestamp.Sub(args.reference) > args.window {
				break
			}
			// if the log is in the cache, then skip it to prevent matching the same log to multiple reference
			// logs and creating duplicate events.
			if logCache.Has(log.Id) {
				continue
			}

			// if we find a log that is after the reference log timestamp, within the configured window,
			// and not in the cache, then we can consider it a match and add it to the list of matched logs to return.
			matchedLogs = append(matchedLogs, log)

			// update the cache to mark this log as processed so that it won't be matched to another reference log \
			// in the future and create duplicate events.
			logCache.Set(log.Id, struct{}{})

			found = true
			break
		}

		if !found {
			logp.Warn("MatchSlabLogs: no matching log found for key: %s", key)
		}
	}

	if len(matchedLogs) == 0 {
		return nil, ErrNoLogsFound
	}

	// sort the matched logs by timestamp in ascending order so that they are in the correct order when added to the event
	// for analysis and indexing in elasticsearch.
	slices.SortFunc(matchedLogs, func(a, b analytics.Source) int {
		return a.Device.Timestamp.Compare(b.Device.Timestamp)
	})

	return matchedLogs, nil
}

// MatchSchedulerRouteLog matches the scheduler route request log for a given reference log by comparing the timestamps of the scheduler logs
// to the timestamp of the reference log and ensuring that they are within the configured time window and not
// in the cache. It returns the matched scheduler route request log.
func MatchSchedulerRouteLog(args *MatchLogArgs) (*analytics.Source, error) {
	var matchedLogs *analytics.Source

	// get the keys from args.logCollection that contain "scheduler"
	for key, logs := range args.logCollection {
		if !strings.Contains(key, "scheduler") {
			continue
		}

		for _, log := range logs {
			// if the scheduler log timestamp is after the reference log timestamp then break out of the loop for this key
			// since the logs are returned in ascending order by timestamp and there won't be any more logs that match for this key.
			if log.Device.Timestamp.After(args.reference) {
				break
			}

			// if the scheduler log timestamp is before the reference log timestamp but outside the
			// configured window, then continue for this key since the logs are returned in ascending order by timestamp
			if args.reference.Sub(log.Device.Timestamp) > args.window {
				continue
			}

			// if the log is in the cache, then skip it to prevent matching the same log to multiple reference
			// logs and creating duplicate events.
			if logCache.Has(log.Id) {
				continue
			}

			// if we find a log that is before the reference log timestamp, within the configured window, and not in the cache,
			// then we can consider it a match and set it as the matched log to return.
			matchedLogs = &log

			// update the cache to mark this log as processed so that it won't be matched to another reference log
			// in the future and create duplicate events.
			logCache.Set(log.Id, struct{}{})

			break
		}
	}

	if matchedLogs == nil {
		return nil, ErrNoLogsFound
	}

	return matchedLogs, nil
}
