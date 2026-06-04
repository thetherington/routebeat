package beater

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"
	"github.com/go-co-op/gocron/v2"
	"github.com/hasura/go-graphql-client"
	"github.com/hasura/go-graphql-client/pkg/jsonutil"

	"github.com/thetherington/routebeat/beater/analytics"
	"github.com/thetherington/routebeat/beater/cache"
	"github.com/thetherington/routebeat/beater/httpclient"
	routeCfg "github.com/thetherington/routebeat/config"
)

const (
	CLIENT_TIMEOUT = 10 // time in seconds
)

var (
	db              analytics.SearchInterface
	scheduler       gocron.Scheduler
	logCache        = cache.NewCacheMap[string, any](40 * time.Minute)
	busRoutingCache = cache.NewCacheMap[string, SlabMap](0)

	lastNotificationTime time.Time
)

// routebeat configuration.
type routebeat struct {
	done       chan struct{}
	config     routeCfg.Config
	client     beat.Client
	httpClient *http.Client
	subClient  *graphql.SubscriptionClient
	subIds     []string
}

// New creates an instance of routebeat.
func New(b *beat.Beat, cfg *config.C) (beat.Beater, error) {
	c := routeCfg.DefaultConfig
	if err := cfg.Unpack(&c); err != nil {
		return nil, fmt.Errorf("error reading config file: %v", err)
	}

	// Initialize the scheduler
	sched, _ := gocron.NewScheduler()
	scheduler = sched

	scheduler.Start()

	// Validate there is atleast 1 tag
	if len(c.Tags) < 1 {
		return nil, errors.New("beat requires atleast 1 tag in the configuration")
	}

	if len(c.PhysicalRouteTags) < 1 {
		return nil, errors.New("beat requires atleast 1 physical route tag in the configuration")
	}

	// Validate if mapping is enabled then the nameset is not blank
	if c.Mapping != nil && c.Mapping.Nameset == "" {
		return nil, errors.New("nameset cannot be blank if mapping is enabled")
	}

	done := make(chan struct{})

	// Create the elasticsearch client handler if ES configuration is provided in the config
	if c.ES != nil {
		var err error
		db, err = analytics.NewClient(&analytics.ClientConfig{
			Address:               c.ES.Address,
			Index:                 c.ES.Index,
			Strict:                c.ES.Strict,
			UseLogIngestTimestamp: c.ES.Dev.UseLogIngestTimestamp,
			TimeZoneFix:           c.ES.Dev.SwapSlabLogTimezone,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create elasticsearch client: %w", err)
		}
	}

	// create generic http client interface and authenticate with magnum
	// http client contains a cookieJar that is updated by a goroutine
	client, err := httpclient.NewHTTPClient(&httpclient.MagnumAuthCredentials{
		ClientID:     c.API.Auth.ClientID,
		ClientSecret: c.API.Auth.ClientSecret,
		TokenURL:     c.API.Auth.TokenURL,
		Done:         done,
	})
	if err != nil {
		return nil, fmt.Errorf("error authenticating with magnum: %v", err)
	}

	bt := &routebeat{
		done:       done,
		config:     c,
		httpClient: client,
		subIds:     make([]string, 0),
	}

	return bt, nil
}

// Run starts routebeat.
func (bt *routebeat) Run(b *beat.Beat) error {
	logp.Info("routebeat is running! Hit CTRL-C to stop it.")

	var err error
	bt.client, err = b.Publisher.Connect()
	if err != nil {
		return err
	}

	// create the subscription client whether it's needed or not
	bt.subClient = graphql.
		NewSubscriptionClient(getWssURL(bt.config.API.Url)).
		WithWebSocketOptions(graphql.WebsocketOptions{
			HTTPClient: bt.httpClient,
		}).
		OnError(func(sc *graphql.SubscriptionClient, err error) error {
			logp.Err("subscription client OnError: %v", err)
			return err
		}).
		OnDisconnected(func() {
			logp.Warn("subscription client disconnected")
		}).
		OnConnected(func() {
			logp.Info("subscription client connected")
		}).
		OnSubscriptionComplete(func(sub graphql.Subscription) {
			logp.Info("subcription terminated %s", sub.GetID())
		})
	defer bt.subClient.Close()

	// check if subscriptions are enabled in the config and subscribe to tags
	if bt.config.API.Notifications {
		var queryForSubs SubscriptionTerminalsUpdated

		// make a subscription query for each tag
		for _, tag := range bt.config.Tags {
			subscriptionId, err := bt.SubscribeTerminals(queryForSubs, tag, Notification)
			if err != nil {
				logp.Err("error creating subscription for Tag: %s", tag)
				continue
			}

			bt.subIds = append(bt.subIds, subscriptionId)
		}

		var queryForPhysical SubscriptionTerminalsUpdated

		// make a subscription query for physical route updates for each physical route tag
		for _, tag := range bt.config.PhysicalRouteTags {
			subscriptionId, err := bt.SubscribeTerminals(queryForPhysical, tag, Scan)
			if err != nil {
				logp.Err("error creating physical subscription for Tag: %s", tag)
				continue
			}

			bt.subIds = append(bt.subIds, subscriptionId)
		}

		// start the subscriptions in the background make it reconnect on connnection lost
		go bt.SubscriptionClientRun()
	}

	// schedule a daily job to clear the bus routing cache to prevent memory bloat from old physical route
	// information that may never be used again after a certain amount of time since physical route information
	// is only useful for enriching events for a certain amount of time before it becomes stale and irrelevant for enriching new events
	scheduler.NewJob(
		gocron.DailyJob(1, gocron.NewAtTimes(gocron.NewAtTime(6, 15, 55))), // schedule to run daily at 6:15:55am UTC
		gocron.NewTask(func() {
			logp.Info("Daily Bus Routing cache clear, clearing bus routing cache to prevent memory bloat")
			busRoutingCache.Clear()
		}),
	)

	// schedule a periodic job to clean up the log cache by removing expired items to prevent memory bloat
	// from old log entries that will never be matched to events since we are past the time window for matching logs to events
	scheduler.NewJob(
		gocron.DurationJob(60*time.Minute), // run every 60 minutes
		gocron.NewTask(func() {
			logp.Info("Periodic Log Cache cleanup, cleaning up log cache to prevent memory bloat")
			logCache.Cleanup()
		}),
	)

	// start a go routine to monitor the time since the last notification was received
	// and close the subscription client if it has been more than 60 minutes since the
	// last notification was received to resubscribe to the subscriptions and run the reconnect logic of the subscription client
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()

		// initialize the last notification time to now so that we don't
		// immediately close the subscription client on startup before we receive any notifications
		lastNotificationTime = time.Now()

		for {
			select {
			case <-bt.done:
				return
			case <-ticker.C:
				// if it has been more than 120 minutes since the last notification was received, then close the subscription client
				if time.Since(lastNotificationTime) > 120*time.Minute {
					logp.Info("closing subscription client to test reconnect logic, last notification received at: %s", lastNotificationTime.Format(time.RFC3339))
					bt.subClient.Close()

					// reset the last notification time to now after closing the subscription client so
					// that we don't immediately close it again in the next tick before we receive any notifications
					lastNotificationTime = time.Now()
				}
			}
		}
	}()

	// block here until the application is terminated
	<-bt.done

	return nil
}

// Stop stops routebeat.
func (bt *routebeat) Stop() {
	bt.client.Close()

	// unsubscribe from all subscriptions
	for _, id := range bt.subIds {
		if err := bt.subClient.Unsubscribe(id); err != nil {
			logp.Err("error unsubscribing from graphql query with id: %s", id)
		}
	}

	// Stops go routines:
	//   - magnum token refresh
	//   - query terminals routine
	//   - subscription run reconnect routine
	//   - exits beat run function
	close(bt.done)

	scheduler.Shutdown()
}

func (bt *routebeat) SubscriptionClientRun() {
	for {
		select {
		case <-bt.done:
			logp.Warn("exiting GraphQL subscription client Run() routine")
			return
		default:
		}

		if err := bt.subClient.Run(); err != nil {
			logp.Err("subscription client Run error: %v", err)
		}

		if len(bt.subClient.GetSubscriptions()) == 0 {
			logp.Warn("exiting GraphQL subscription client Run() routine")
			return
		}

		logp.Info("subscription client reconnect/re-run")
	}
}

// SubscribeTerminals subscribes to the route subscribe notifications for a given tag and event type (Notification or Scan)
// and processes the incoming messages to build events for the beat. The event type is used to determine how to process the
// incoming messages and what type of events to build.
func (bt *routebeat) SubscribeTerminals(query any, tag string, eventType EventType) (string, error) {
	// variables
	v := map[string]any{
		"tag":   tag,
		"isSub": (eventType == Notification), // if event type is Notification then isSub should be true, otherwise false
	}

	// subscribe to a query and run a callback function to process the messages
	id, err := bt.subClient.Subscribe(query, v, func(message []byte, err error) error {
		if err != nil {
			return err
		}

		// update the last notification time to now since we just received a notification for this tag.
		// This will be used to determine if we have received any notifications in the last 60 minutes
		lastNotificationTime = time.Now()

		data := SubscriptionTerminalsUpdated{}

		// unmarshal message payload
		if err := jsonutil.UnmarshalGraphQL(message, &data); err != nil {
			logp.Err("failed to unmarshal subscription response for Tag:%s %v", tag, err)
			return nil
		}

		switch eventType {
		case Notification:
			bt.BuildEvents(tag, data.TerminalsUpdated, Notification)
		case Scan:
			bt.ScanEvents(data.TerminalsUpdated)
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	logp.Info("Subscription made for Tag: %s with Sub ID: %s", tag, id)

	return id, nil
}

// ScanEvents is used to process the physical route update subscription events and update the bus routing cache
// with the latest physical route information for each subscribed source. This cache is then used to enrich the
// route subscribe notifications with physical route information.
func (bt *routebeat) ScanEvents(edges []Edge) {
	logp.Debug("ScanEvents", "Scanning %d events for physical route updates", len(edges))

	for _, edge := range edges {
		var (
			deviceName string
			output     int
			subName    string
		)

		// if there is a subscribed source, then find the nameset value for the subscribed source to use as the key
		// in the bus routing cache. If there is no subscribed source, then we can't add this physical route information
		// to the cache because we won't know which subscribed source it belongs to.
		if edge.SubscribedSource != nil {
			subName = findNamesetValueByName(
				bt.config.Mapping.Nameset,
				edge.SubscribedSource.NamesetNames,
				bt.config.Mapping.Default,
			)
		}

		// if we can't find a nameset value for the subscribed source, then we won't be able to match this physical route information
		// to any subscribed sources in the route subscribe notifications, so we should skip adding this physical route information to the cache since it won't be useful for enriching any events.
		if subName == "" {
			continue
		}

		if edge.Port != nil {
			deviceName = edge.Port.Device.Name

			outputInt, err := ExtractOutputFromPort(edge.Port.Id)
			if err != nil {
				logp.Err("failed to extract output from port Id: %s, error: %v", edge.Port.Id, err)
				continue
			}

			output = outputInt
		}

		p := SlabPartial{
			Id:     edge.Id,
			Name:   edge.Name,
			Device: deviceName,
			Output: output,
		}

		// if there is already a slab partial for this subscribed source,
		// then add this physical route to the slab partial map for this subscribed source.
		// otherwise, create a new slab partial map for this subscribed source with this physical route.
		if busRoutingCache.Has(subName) {
			busRoutingCache.DoMut(subName, func(value SlabMap) {
				value[edge.Id] = p
			})
		} else {
			busRoutingCache.Set(subName, SlabMap{edge.Id: p})
		}

		logp.Debug("ScanEvents", "ScanEvent for: %s (%s) Output: %d SubscribedSource: %s", edge.Name, deviceName, output, subName)
	}
}

/*
Route Subscribe Notification
|
|-- Scheduled Task to Analyze Logs
	|
	|-- AnalyzeLogCollection
		|
		|-- Query Magnum Logs
		|
		|-- Query Additional Logs
		|
		|-- Match Logs to Event
			|
			|-- If matched, enrich event with log data and publish
*/

// Builds beat events based on the Edge{} struct definition
func (bt *routebeat) BuildEvents(tag string, edges []Edge, eventType EventType) {
	logp.Debug("ProcessResults", "Query Results for Tag: %s, Total Edges:%d, EventType %s", tag, len(edges), eventType)

	var (
		events    []beat.Event // slice to hold the events we will publish after processing all edges
		buscodes  []string     // for logging purposes to track which bus codes we are processing events for
		processed int          // count of how many events were processed (matched the tag and created an event)
		discarded int          // count of how many events were discarded because they didn't match the tag
	)

	// check if mapping is enabled to determine if we need to find the nameset value for the source and destination labels
	var mapping bool = bt.config.Mapping != nil

	for _, edge := range edges {
		// check if the tag exactly matches one of the items in tags
		// known magnum issue with graphql filters not matching the full tag value
		if !slices.Contains(edge.Tags, tag) {
			discarded++
			continue
		}

		// build basic event from query payload
		event := beat.Event{
			Timestamp: time.Now(),
			Fields: mapstr.M{
				"dstId":             edge.Id,
				"dstName":           edge.Name,
				"dstIsSub":          edge.IsSub,
				"dstIsDst":          edge.IsDst,
				"dstType":           edge.Type,
				"dstTags":           edge.Tags,
				"dstTag":            tag,
				"dstNameset":        mapstr.M{},
				"eventType":         eventType.String(),
				"routeableTerminal": mapstr.M{},
				"matched":           false, // field to indicate if the event was matched to a log in the 5 minute window
			},
		}

		// put in the nameset name and value "event.nameset.<nameset name>"
		for _, n := range edge.NamesetNames {
			event.PutValue(
				fmt.Sprintf("dstNameset.%s", strings.ToLower(n.Nameset.Name)),
				n.Name,
			)
		}

		// create "source" and "destination" keys in the event if mapping is enabled
		if mapping {
			event.PutValue("destinationLabel", findNamesetValueByName(
				bt.config.Mapping.Nameset,
				edge.NamesetNames,
				bt.config.Mapping.Default),
			)

			event.PutValue("sourceLabel", bt.config.Mapping.Default)
		}

		// create a nested object for the RoutePhysicalSource
		if edge.RoutedPhysicalSource != nil {
			m := mapstr.M{
				"srcId": edge.RoutedPhysicalSource.Id,
				"name":  edge.RoutedPhysicalSource.Name,
				"isSrc": edge.RoutedPhysicalSource.IsSrc,
			}

			if len(edge.RoutedPhysicalSource.Tags) > 0 {
				m.Put("physTags", edge.RoutedPhysicalSource.Tags)
			}

			rangeOverNamesets(edge.RoutedPhysicalSource.NamesetNames, &m)

			event.PutValue("routeableTerminal.physicalSource", m)

			if mapping {
				event.PutValue("sourceLabel", findNamesetValueByName(
					bt.config.Mapping.Nameset,
					edge.RoutedPhysicalSource.NamesetNames,
					bt.config.Mapping.Default,
				))
			}
		}

		// if there is a port object in the routed physical source, then add the port and device information to the event
		if edge.RoutedPhysicalSource != nil && edge.RoutedPhysicalSource.Port != nil {
			for _, a := range edge.RoutedPhysicalSource.Port.Addresses {
				if !a.Backup {
					event.PutValue("routeableTerminal.physicalSource.port", mapstr.M{
						"address":    a.Name,
						"streamType": a.StreamType,
						"port":       edge.RoutedPhysicalSource.Port.Name,
					})
				}
			}
		}

		// create a nested object for the SubscribedSource
		if edge.SubscribedSource != nil {
			m := mapstr.M{
				"srcId": edge.SubscribedSource.Id,
				"name":  edge.SubscribedSource.Name,
				"isSub": edge.SubscribedSource.IsSub,
			}

			if len(edge.SubscribedSource.Tags) > 0 {
				m.Put("subTags", edge.SubscribedSource.Tags)
			}

			rangeOverNamesets(edge.SubscribedSource.NamesetNames, &m)

			event.PutValue("routeableTerminal.subscribedSource", m)

			if mapping {
				event.PutValue("sourceLabel", findNamesetValueByName(
					bt.config.Mapping.Nameset,
					edge.SubscribedSource.NamesetNames,
					bt.config.Mapping.Default,
				))
			}
		}

		// destination has no physical source or subscribed source then remove the key
		if edge.RoutedPhysicalSource == nil && edge.SubscribedSource == nil {
			event.Delete("routeableTerminal")
		}

		if dst, err := event.GetValue("destinationLabel"); err == nil {
			buscodes = append(buscodes, dst.(string))
		}

		events = append(events, event)

		processed++
	}

	// early return if no events were processed to avoid scheduling unnecessary jobs and log messages
	if len(events) == 0 {
		return
	}

	// schedule a job to analyze the logs for these events after the configured delay and then publish the enriched events to the output
	scheduler.NewJob(
		gocron.OneTimeJob(
			gocron.OneTimeJobStartDateTime(time.Now().Add(bt.config.ES.Delay)),
		),
		gocron.NewTask(bt.ScheduledTaskLogsAnalyze, events),
		gocron.WithLimitedRuns(1),
	)

	logp.Debug("ProcessResults", "Scheduled %d events for Tag: %s with for: [%v], EventType: %s", len(events), tag, strings.Join(buscodes, ","), eventType)
}

// ScheduledTaskLogsAnalyze is the function that is scheduled to run after the configured delay for each batch of events
// that are built from the query results or subscriptions. This function will analyze the logs for each event and enrich
// the event with log data if there is a match. After analyzing the logs for each event, it will publish the enriched events to the output.
func (bt *routebeat) ScheduledTaskLogsAnalyze(events []beat.Event) {
	for i, event := range events {
		busname, err := ExtractStringFromEvent(&event, "destinationLabel")
		if err != nil || busname == "" {
			continue
		}

		// analyze the logs for this event and enrich the event with log data if there is a match
		if err := bt.AnalyzeLogCollection(&event); err != nil {
			HandleAnalyzeLogError(err, &event)
		}

		// enrich the event with physical route information from the cache if there is a destination label
		// and physical route information in the cache for that destination label
		if slabs, ok := busRoutingCache.Get(busname); ok {
			destinations := make([]any, 0, len(slabs))
			for _, s := range slabs {
				destinations = append(destinations, s)
			}

			event.PutValue("slab_destinations", destinations)
		}

		events[i] = event
	}

	bt.client.PublishAll(events)

	// set events to nil to allow the garbage collector to free up the memory used by the events
	// since we have already published them and we don't need to keep them in memory anymore
	events = nil
}

func (bt *routebeat) AnalyzeLogCollection(event *beat.Event) error {
	type logEntry struct {
		Log  string    `json:"log"`
		Time time.Time `json:"time"`
		Type string    `json:"type"`
	}

	matchedLogMessages := make([]logEntry, 0)

	var (
		haveSlabLogs bool
		haveSchedLog bool
	)

	// defer putting the matched log messages in the event until the end of the function so that we can
	// ensure that we capture all of the matched logs from the different queries and matching functions
	// and put them in the event at once. This will allow us to have a complete set of matched logs for
	// this event when we analyze the logs for keywords and patterns.
	defer func() {
		if len(matchedLogMessages) > 0 {
			event.PutValue("matchedLogs", matchedLogMessages)
		}
	}()

	// extract srcId and dstId from the event for log analysis
	srcId, dstId, err := getSrcDstIds(event)
	if err != nil {
		return fmt.Errorf("failed to get srcId/dstId from event: %v, error: %s", event, err.Error())
	}

	// search for magnum logs that match the source and destination in the event within the configured time window
	magnumLogs, err := ProcessMagnumLogs(&MagnumLogsOptions{
		src:       srcId,
		dst:       dstId,
		sweeping:  bt.config.ES.Sweeping,
		eventTime: event.Timestamp,
		window:    bt.config.ES.Window,
	})
	if err != nil {
		return err
	}

	event.PutValue("matched", true)
	event.PutValue("swept", magnumLogs.Swept)

	// replace the event timestamp with the request log timestamp
	event.Timestamp = magnumLogs.Request.Device.Timestamp

	// put the matched logs in the event under the "matchedLogs" key.
	// This will allow us to easily search for keywords in the log messages across all of the
	// relevant logs for this event without having to search within nested objects in elasticsearch.
	matchedLogMessages = append(matchedLogMessages,
		logEntry{Log: magnumLogs.Request.Log.Syslog.Message, Time: magnumLogs.Request.Device.Timestamp, Type: "magnum"},
		logEntry{Log: magnumLogs.Complete.Log.Syslog.Message, Time: magnumLogs.Complete.Device.Timestamp, Type: "magnum"},
	)

	// collect the logs for each slab in the physical route as well as the scheduler logs for the overall route
	// by executing multi-search queries using the QueryLogsFromEvent function.
	// The QueryLogsFromEvent function will return a map of log results for each query keyed by the query's Key() value.
	// If there are no results found for any of the queries, then return an error since we won't have any logs to analyze for this event.
	logCollection, err := QueryLogsFromEvent(context.Background(), event)
	if err != nil {
		return err
	}

	// match the slab logs for this event by comparing the timestamps of the slab logs to the timestamp of the
	// route completion log and ensuring that they are within the configured time window and not in the cache.
	// If there are no matched slab logs found, then return an error since we won't have any logs to analyze for this event.
	slabLogs, err := MatchSlabLogs(&MatchLogArgs{
		logCollection: logCollection,
		reference:     magnumLogs.Request.Device.Timestamp,
		window:        bt.config.ES.Window,
	})
	if err == nil {
		for _, s := range slabLogs {
			matchedLogMessages = append(matchedLogMessages,
				logEntry{
					Log:  fmt.Sprintf("%s %s", s.Annotation.General.DeviceName, s.Log.Syslog.Message),
					Time: s.Device.Timestamp, Type: "slab",
				},
			)
		}

		haveSlabLogs = true
	}

	// match the scheduler route request log for this event by comparing the timestamps of the scheduler logs to the
	// timestamp of the route request log and ensuring that they are within the configured time window and not in the cache.
	// If there is no matched scheduler route request log found, then log a debug message but do not return an error since we
	// can still analyze the slab and magnum logs for this event without the scheduler logs. add the log to the begining of the matchedLogMessages slice
	schedulerLog, err := MatchSchedulerRouteLog(&MatchLogArgs{
		logCollection: logCollection,
		reference:     magnumLogs.Request.Device.Timestamp,
		window:        5 * time.Second, // smaller window for matching the scheduler log since it should be very close to the request log timestamp
	})
	if err == nil {
		matchedLogMessages = append([]logEntry{
			{Log: schedulerLog.Log.Syslog.Message, Time: schedulerLog.Device.Timestamp, Type: "scheduler"},
		}, matchedLogMessages...)

		// replace the event timestamp with the scheduler log timestamp since this is when the route began
		event.Timestamp = schedulerLog.Device.Timestamp
		haveSchedLog = true
	}

	// if we have both slab logs and a scheduler log,
	// then we can calculate the duration of the route from the scheduler log timestamp to the
	// last slab log timestamp and put that in the event under the "durationMs" key.

	// This will allow us to easily search for long duration routes in elasticsearch
	// without having to analyze the log messages for keywords related to long durations.
	if haveSlabLogs && haveSchedLog {
		// calculate the duration between the request and complete logs
		first := matchedLogMessages[0].Time
		last := matchedLogMessages[len(matchedLogMessages)-1].Time

		event.PutValue("durationMs", last.Sub(first).Milliseconds())
	}

	return nil
}
