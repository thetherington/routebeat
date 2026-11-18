package beater

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"
	"github.com/hasura/go-graphql-client"
	"github.com/hasura/go-graphql-client/pkg/jsonutil"

	"github.com/thetherington/routebeat/beater/cache"
	"github.com/thetherington/routebeat/beater/eventsapi"
	"github.com/thetherington/routebeat/beater/httpclient"
	"github.com/thetherington/routebeat/beater/notify"
	"github.com/thetherington/routebeat/beater/schedule"
	routeCfg "github.com/thetherington/routebeat/config"
)

const (
	BUSCACHE_FILE   = "busCache.gob"
	SCHEDULE_FILE   = "scheduleStore.gob"
	GLOBAL_COUNTERS = "global"

	SCHEDULE_PRUNE_PERIOD = 10 * time.Minute
)

var (
	notifier notify.NotiferInterface

	scheduleStore = schedule.NewStore()
	countersCache = cache.NewCacheMap[string, *Counters]()
	busCache      = cache.NewCacheMap[string, *BusState]()

	lastNotificationTime time.Time
)

// noopNotifier is used when no notify configuration is found so transitions don't need a nil check
type noopNotifier struct{}

func (noopNotifier) Send(...notify.Notification) error { return nil }
func (noopNotifier) GetOrigin() string                 { return "" }

// routebeat configuration.
type routebeat struct {
	done       chan struct{}
	config     routeCfg.Config
	client     beat.Client
	httpClient *http.Client
	subClient  *graphql.SubscriptionClient
	subIds     []string
	queryMu    sync.Mutex
	eventsAPI  *eventsapi.Client // schedule source. only nil in tests
}

// New creates an instance of routebeat.
func New(b *beat.Beat, cfg *config.C) (beat.Beater, error) {
	c := routeCfg.DefaultConfig
	if err := cfg.Unpack(&c); err != nil {
		return nil, fmt.Errorf("error reading config file: %v", err)
	}

	// Validate there is atleast 1 tag
	if len(c.Tags) < 1 {
		return nil, errors.New("beat requires atleast 1 tag in the configuration")
	}

	// Validate if mapping is enabled then the nameset is not blank
	if c.Mapping != nil && c.Mapping.Nameset == "" {
		return nil, errors.New("nameset cannot be blank if mapping is enabled")
	}

	// the elasticsearch schedule source and the webhook server were removed
	for _, removed := range []string{"elasticsearch", "webhook"} {
		if cfg.HasField(removed) {
			logp.Warn("ignoring removed config block: %s", removed)
		}
	}

	// create the events api client used to poll the schedule and refresh a destination on demand
	eventsAPI, err := eventsapi.New(eventsapi.Config{
		Url:         c.EventsAPI.Url,
		Timeout:     c.EventsAPI.Timeout,
		BulkTimeout: c.EventsAPI.BulkTimeout,
		Headers:     c.EventsAPI.Headers,
		StartParam:  c.EventsAPI.StartParam,
		EndParam:    c.EventsAPI.EndParam,
		OutputParam: c.EventsAPI.OutputParam,
		TaskParam:   c.EventsAPI.TaskParam,
		Task:        c.EventsAPI.Task,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create events api client: %w", err)
	}

	logp.Info("Schedule source: events api %s (period %s, lookahead %s)", c.EventsAPI.Url, c.Schedule.Period, c.Schedule.Lookahead)

	// try to load the busCache from file
	if err := busCache.LoadFromFile(BUSCACHE_FILE); err != nil {
		logp.Err("failed to load busCache from file: %v", err)
	} else {
		logp.Info("Loaded busCache length: %d keys", busCache.Length())
	}

	// try to load the schedule store from file, so the schedule is available before the first poll
	if err := scheduleStore.LoadFromFile(SCHEDULE_FILE); err != nil {
		logp.Err("failed to load schedule store from file: %v", err)
	} else {
		// drop windows that expired while the beat was not running
		windows, _ := scheduleStore.Prune(time.Now())
		logp.Info("Loaded schedule store: %d outputs (%d expired windows removed)", scheduleStore.Length(), windows)
	}

	if c.Schedule.Dev.LoadCache {
		logp.Info("schedule.dev.load_cache is set, the events api schedule poll is disabled")
	}

	done := make(chan struct{})

	// create generic http client interface and authenticate with magnum
	// http client contains a cookieJar that is updated by a goroutine
	client, err := httpclient.NewHTTPClient(httpclient.WithMagnumAuth(
		&httpclient.MagnumAuthCredentials{
			ClientID:     c.API.Auth.ClientID,
			ClientSecret: c.API.Auth.ClientSecret,
			TokenURL:     c.API.Auth.TokenURL,
			Done:         done,
		},
	))
	if err != nil {
		return nil, fmt.Errorf("error authenticating with magnum: %v", err)
	}

	// create notify app if Manual settings or AutoDiscovery settings are present
	if len(c.Notifiers.Manual) > 0 {
		nodes, _ := ConvertSliceJSON[routeCfg.Node, notify.Node](c.Notifiers.Manual)

		notifier, err = notify.NewNotifierApp(notify.WithManualConfig(&notify.NotifyAppCfg{
			Nodes:  nodes,
			Origin: b.Info.Hostname,
		}))
		if err != nil {
			return nil, fmt.Errorf("failed to create notify app: %v", err)
		}

		logp.Info("Created notify app with %d nodes", len(nodes))

		// Auto discovery settings
	} else if c.Notifiers.Auto.Host != "" {
		notifier, err = notify.NewNotifierApp(notify.WithAutoDiscover(&notify.AutoDiscoveryCfg{
			Host:          c.Notifiers.Auto.Host,
			Username:      c.Notifiers.Auto.Username,
			Password:      c.Notifiers.Auto.Password,
			NotifierTypes: c.Notifiers.Auto.NotifierTypes,
			Origin:        b.Info.Hostname,
		}))
		if err != nil {
			return nil, fmt.Errorf("failed to create notify app: %v", err)
		}

		logp.Info("Created notify app with auto configuration for host: %s", c.Notifiers.Auto.Host)

		// no notify configuration found
	} else {
		logp.Warn("No notify configuration found. Notifications will be disabled")
		notifier = noopNotifier{}
	}

	bt := &routebeat{
		done:       done,
		config:     c,
		httpClient: client,
		subIds:     make([]string, 0),
		eventsAPI:  eventsAPI,
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

	// start the events api schedule poll routine (5 minutes default)
	if !bt.config.Schedule.Dev.LoadCache {
		go ScheduleQueryGoRoutine(bt.eventsAPI, bt.config.Schedule.Period, bt.config.Schedule.Lookahead, bt.done)
	}

	// go routine to save the busCache and the schedule store to file every 10 minutes incase of a crash
	go SaveCachesGoRoutine(bt.done)

	// create the graphql query client
	queryClient := graphql.NewClient(bt.config.API.Url, bt.httpClient)

	// run the query client in a seperate go routine for each tag
	for _, tag := range bt.config.Tags {
		// set counters cache for query
		countersCache.Set(tag, &Counters{})

		// run the query routine in the background
		go bt.QueryTerminalsRoutine(queryClient, tag, bt.done)
	}

	// create the subscription client whether it's needed or not
	bt.subClient = graphql.
		NewSubscriptionClient(getWssURL(bt.config.API.Url)).
		WithWebsocketConnectionIdleTimeout(1 * time.Minute).
		WithWebSocketOptions(graphql.WebsocketOptions{
			HTTPClient: bt.httpClient,
		}).
		OnError(func(sc *graphql.SubscriptionClient, err error) error {
			if sc.IsWebsocketConnectionIdleTimeout(err) {
				logp.Warn("Subscription went stale (Idle Timeout)! Re-initiating...")
				// Returning nil tells the client's internal loop to attempt a retry
				return nil
			}

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
		var query SubscriptionTerminalsUpdated

		// make a subscription query for each tag
		for _, tag := range bt.config.Tags {
			subscriptionId, err := bt.SubscribeTerminals(query, tag)
			if err != nil {
				logp.Err("error creating subscription for Tag: %s", tag)
				continue
			}

			bt.subIds = append(bt.subIds, subscriptionId)
		}

		// start the subscriptions in the background make it reconnect on connnection lost
		go bt.SubscriptionClientRun()
	}

	// start a go routine to monitor the time since the last notification was received
	// and close the subscription client if it has been more than 60 minutes since the
	// last notification was received to resubscribe to the subscriptions and run the reconnect logic of the subscription client
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		// initialize the last notification time to now so that we don't
		//  immediately close the subscription client on startup before we receive any notifications
		lastNotificationTime = time.Now()

		for {
			select {
			case <-bt.done:
				return
			case <-ticker.C:
				// if it has been more than 120 minutes since the last notification was received, then close the subscription client
				if time.Since(lastNotificationTime) > bt.config.API.SubReconnectIdle {
					logp.Info("closing subscription client to test reconnect logic, last notification received at: %s", lastNotificationTime.Format(time.RFC3339))
					bt.subClient.Close()

					// reset the last notification time to now after closing the subscription client so
					// that we don't immediately close it again in the next tick before we receive any notifications
					lastNotificationTime = time.Now()
				}
			}
		}
	}()

	// remove expired windows from the schedule store every SCHEDULE_PRUNE_PERIOD
	go PruneScheduleRoutine(bt.done)

	// periodically log scheduler event counters for visibility during runtime
	go logScheduleEventCounters(bt.done)

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
	//   - schedule prune routine
	//   - log schedule event counters routine
	//   - events api schedule poll
	//   - busCache/schedule store SaveToFile
	//   - query terminals routine
	//   - subscription run reconnect routine
	//   - exits beat run function
	close(bt.done)
}

func (bt *routebeat) QueryTerminalsRoutine(client *graphql.Client, tag string, done chan struct{}) {
	// variables
	variables := map[string]any{
		"tag":   strconv.Quote(tag),
		"limit": bt.config.API.Limit,
	}

	ticker := time.NewTicker(bt.config.Period)

	for {
		select {
		case <-bt.done:
			logp.Warn("exiting QueryTerminalsRoutine for Tag: %s", tag)
			return
		case <-ticker.C:
		}

		var query QueryTerminals

		err := func() error {
			// If enabled, serialize query requests across all tags to avoid overloading GraphQL.
			if bt.config.API.SerializeQueries {
				bt.queryMu.Lock()
				defer bt.queryMu.Unlock()
			}

			ctx, cancel := context.WithTimeout(context.Background(), bt.config.API.Timeout)
			defer cancel()

			return client.Query(ctx, &query, variables)
		}()
		if err != nil {
			logp.Err("error query failed for Tag: %s: %v", tag, err)
			continue
		}

		// check if there has been results to process
		if query.Terminals.TotalCount < 1 {
			logp.Info("Query Results is 0 for Tag: %s", tag)
			continue
		}

		// process results
		bt.BuildEvents(tag, query.Terminals.Edges, Query)
	}
}

func (bt *routebeat) SubscribeTerminals(query any, tag string) (string, error) {
	// variables
	v := map[string]any{
		"tag": strconv.Quote(tag),
	}

	// subscribe to a query and run a callback function to process the messages
	id, err := bt.subClient.Subscribe(query, v, func(message []byte, err error) error {
		if err != nil {
			return err
		}

		// update the last notification time to now whenever a new notification is received
		lastNotificationTime = time.Now()

		data := SubscriptionTerminalsUpdated{}

		// unmarshal message payload
		if err := jsonutil.UnmarshalGraphQL(message, &data); err != nil {
			logp.Err("failed to unmarshal subscription response for Tag:%s %v", tag, err)
			return nil
		}

		bt.BuildEvents(tag, data.TerminalsUpdated, Notification)

		return nil
	})
	if err != nil {
		return "", err
	}

	logp.Info("Subscrition made for Tag: %s with Sub ID: %s", tag, id)

	return id, nil
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

// Builds beat events based on the Edge{} struct definition
func (bt *routebeat) BuildEvents(tag string, edges []Edge, eventType EventType) {
	selector := processResultsSelector(eventType)

	logp.Debug(selector, "Query Results for Tag: %s, Total Edges:%d, EventType %s", tag, len(edges), eventType)

	var (
		events []beat.Event

		processed int // counter for any routes with matched exact tag
		discarded int // counter for any routes with unmatched exact tag
		unmatched int // counter for any destinations not in the cache
		expired   int // counter for any destination expired schedule end time

		counters      = &Counters{Tag: tag}            // counters for the schedule state routing
		notifications = make([]notify.Notification, 0) // notifications to send
		destinations  []string                         // destination labels in a notification batch, for the debug logs
	)

	for _, edge := range edges {
		// create beat event from edge information and update counters on route deviation findings
		results, err := bt.CreateEventFromEdge(&edge, tag, counters, eventType)
		if err != nil {
			switch {
			case errors.Is(err, ErrEdgeTagNotFound):
				discarded++
				continue

			case errors.Is(err, ErrScheduleBusNotFound):
				unmatched++

			case errors.Is(err, ErrScheduleBusExpired):
				unmatched++
				expired++
			}
		}

		events = append(events, *results.event)
		notifications = append(notifications, results.notifications...)

		if eventType == Notification {
			destinations = append(destinations, eventDestination(results.event))
		}

		processed++
	}

	// notification batches list their destinations in the debug logs, query batches only log totals
	var destinationsLog string
	if eventType == Notification {
		destinationsLog = ", Destinations: " + strings.Join(destinations, ",")
	}

	switch eventType {
	case Query:
		// simple query route just use the counters as-is
		countersCache.Set(tag, counters)

	case Notification:
		// optimistic update the counters when notification route occurs
		countersCache.DoMut(tag, func(value *Counters) {
			value.Merge(counters)
		})
	}

	// create the summary event from the cache
	if c, ok := countersCache.Get(tag); ok {
		events = append(events, beat.Event{
			Timestamp:  time.Now(),
			TimeSeries: false,
			Fields: mapstr.M{
				"eventType": Summary.String(),
				"dstTag":    tag,
				"counters": mapstr.M{
					"unmatched":      unmatched,
					"processed":      processed,
					Primary.String(): c.Primary,
					Backup.String():  c.Backup,
					Zorro.String():   c.Zorro,
					TDA.String():     c.Tda,
					Unsched.String(): c.Unsched,
				},
			},
		})
	}

	// update the global counters by merging existing counters into it
	if eventType == Query {
		countersCache.Do(func(c *cache.CacheMap[string, *Counters]) {
			g := new(Counters)

			for _, v := range c.Store {
				g.Merge(v)
			}

			// create the global counters event from the cache
			events = append(events, beat.Event{
				Timestamp:  time.Now(),
				TimeSeries: false,
				Fields: mapstr.M{
					"eventType": Summary.String(),
					"dstTag":    GLOBAL_COUNTERS,
					"counters": mapstr.M{
						Primary.String(): g.Primary,
						Backup.String():  g.Backup,
						Zorro.String():   g.Zorro,
						TDA.String():     g.Tda,
						Unsched.String(): g.Unsched,
					},
				},
			})
		})
	}

	// send any notifications in a seperate goroutine
	if len(notifications) > 0 && notifier != nil {
		go func() {
			if err := notifier.Send(notifications...); err != nil {
				logp.Err("failed to send notifications: %v", err)
				return
			}

			logp.Debug(selector, "Sent %d notifications for Tag: %s, EventType: %s%s", len(notifications), tag, eventType, destinationsLog)
		}()
	}

	bt.client.PublishAll(events)

	logp.Debug(selector, "Tag: %s, Processed: %d, Discarded: %d, Unmatched: %d, Expired: %d, EventType: %s%s",
		tag,
		processed,
		discarded,
		unmatched,
		expired,
		eventType,
		destinationsLog,
	)
}

type EventResults struct {
	event         *beat.Event
	notifications []notify.Notification
}

func (bt *routebeat) CreateEventFromEdge(edge *Edge, tag string, counters *Counters, eventType EventType) (*EventResults, error) {
	var (
		mapping bool = bt.config.Mapping != nil

		dstLabel     string       // broadview destination label
		srcLabel     string       // broadview source label
		prevSrcLabel string       // broadview previous source label
		prevState    RoutingState // placeholder to store the previous routing state (default Unknown)
		currentState RoutingState // placeholder to store the current routing state (default Unknown)

		srcTags       = make([]string, 0) // source tags
		notifications = make([]notify.Notification, 0)
	)

	// check if the tag exactly matches one of the items in tags
	// known magnum issue with graphql filters not matching the full tag value
	if !slices.Contains(edge.Tags, tag) {
		return nil, ErrEdgeTagNotFound
	}

	// build basic event from query payload
	event := beat.Event{
		Timestamp:  time.Now(),
		TimeSeries: false,
		Fields: mapstr.M{
			"dstId":             edge.Id,
			"dstName":           edge.Name,
			"dstIsSub":          edge.IsSub,
			"dstIsDst":          edge.IsDst,
			"dstType":           edge.Type,
			"dstTags":           edge.Tags,
			"dstTag":            tag,
			"eventType":         eventType.String(),
			"dstNameset":        mapstr.M{},
			"routeableTerminal": mapstr.M{},
			"schedule":          mapstr.M{},
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
		dstLabel = findNamesetValueByName(
			bt.config.Mapping.Nameset,
			edge.NamesetNames,
			bt.config.Mapping.Default,
		)

		event.PutValue("destinationLabel", dstLabel)
		event.PutValue("sourceLabel", bt.config.Mapping.Default)
	}

	// create a nested object for the RoutePhysicalSource
	if edge.RoutedPhysicalSource != nil {
		m := mapstr.M{
			"name":  edge.RoutedPhysicalSource.Name,
			"isSrc": edge.RoutedPhysicalSource.IsSrc,
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

	// create a nested object for the SubscribedSource
	if edge.SubscribedSource != nil {
		m := mapstr.M{
			"name":  edge.SubscribedSource.Name,
			"isSub": edge.SubscribedSource.IsSub,
		}

		rangeOverNamesets(edge.SubscribedSource.NamesetNames, &m)

		event.PutValue("routeableTerminal.subscribedSource", m)

		if mapping {
			srcLabel = findNamesetValueByName(
				bt.config.Mapping.Nameset,
				edge.SubscribedSource.NamesetNames,
				bt.config.Mapping.Default,
			)

			event.PutValue("sourceLabel", srcLabel)
		}

		srcTags = append(srcTags, edge.SubscribedSource.Tags...)
	}

	// destination has no physical source or subscribed source then remove the key
	if edge.RoutedPhysicalSource == nil && edge.SubscribedSource == nil {
		event.Delete("routeableTerminal")
	}

	// serialize this destination: subscription messages are handled concurrently, so a second
	// notification for the bus waits here until the first has finished its recheck and stored
	// the new state. taken before the routing lookup so the waiter sees the refreshed routing
	defer busLocks.Lock(dstLabel)()

	// get the busrouting information from the schedule store based on the destination label
	// return the event as-is if the store doesn't have a routing
	routing, routingSource, ok := GetBusRouting(dstLabel, bt.config.Schedule.Dev.ValidateEndDate)

	// a source that isn't on the active window (or a bus between windows) may be the route for an
	// event about to start: the scheduler routes a few seconds early. use that upcoming window
	if !ok || matchScheduledState(srcLabel, routing) == Unsched {
		if early, found := bt.earlyStartRouting(dstLabel, srcLabel); found {
			routing, routingSource, ok = early, SourceLookahead, true
		}
	}

	if !ok {
		event.PutValue("schedule.matched", false)
		event.PutValue("schedule.routingSource", routingSource.String())
		return &EventResults{event: &event, notifications: notifications}, ErrScheduleBusNotFound
	}

	logp.Debug("ScheduleCache", "Destination:%s Source:%s RoutingSource:%s %+v", dstLabel, srcLabel, routingSource, routing)

	currentState = bt.resolveRoutingState(srcLabel, srcTags, routing)

	// a blank source label can't be resolved. report the unknown status only, without
	// changing the stored bus state, sending notifications or updating the counters
	if currentState == Unknown {
		event.PutValue("schedule", scheduleFields(dstLabel, currentState, routing, routingSource))
		return &EventResults{event: &event, notifications: notifications}, nil
	}

	// get the stored state from the busCache (don't update yet)
	// if the bus code doesn't exist in the cache, create a new BusState with the current state
	if busState, ok := busCache.Get(dstLabel); ok {
		prevState, prevSrcLabel = busState.State, busState.Source
	} else {
		busCache.Set(dstLabel, NewBusState(currentState, srcLabel))
	}

	// a notification that moved a known bus into Unsched may be an advance event or a FF,
	// so refresh the destination from the events api and recheck
	if bt.eventsAPI != nil && isUnscheduledTransition(eventType, prevState, currentState) {
		currentState, routing, routingSource = bt.recheckRoute(dstLabel, srcLabel, routing, routingSource)
	}

	// schedule grouped data
	event.PutValue("schedule", scheduleFields(dstLabel, currentState, routing, routingSource))

	// check if there's a there's a transition
	// create notifications for the state change, decrement the counters based on the prevState, and add the deviationStartTime field to the transition time
	// update the state in the busCache with the current state and updated source label.  This is done in the mutex lock to avoid
	if (eventType == Notification) && (prevState != currentState) {
		// perform this in a mutex lock - for concurrency reasons
		busCache.DoMut(dstLabel, func(value *BusState) {
			// set the current time to now and save previous time.
			prevTime := value.SetTransitionTime(time.Now(), FlagPreviousTime)

			// For notification type events:
			//   1: generate notifications for the state change
			//   2: decrement the counters based on the prevState
			//   3: add the deviationStartTime field to the transition time
			//   4: if the new status is primary, reset the transition time, add deviationEndTime
			// Build Notifications Transition - close the previous status and open the current status
			notifications = append(notifications, transitionNotifications(stateTransition{
				busname:    dstLabel,
				eventType:  eventType,
				trigger:    "GraphQL Subscription",
				tags:       edge.Tags,
				prevState:  prevState,
				prevSource: prevSrcLabel,
				prevStart:  prevTime,
				state:      currentState,
				source:     srcLabel,
				time:       value.GetTransitionTimeStr(),
			})...)

			// optimistic update the counters
			counters.Decrement(prevState)

			// get the transition start time always for every state change
			event.PutValue("schedule.deviationStartTime", value.GetTransitionTimeStr())

			// if the state has switched to primary and this is a notification,
			// then update the deviation end time. reset the transition time to nil
			// and store the current time into the restore.  get the transition begin time as the start time
			// it should be the time when the transition originally left from primary.
			if currentState == Primary {
				event.PutValue("schedule.deviationStartTime", value.GetTransitionTimeStr(FlagTransitionStart, FlagOnlyTransition))
				event.PutValue("schedule.deviationEndTime", value.ResetTransition())
			}

			// update the busState cache with the current state and updated source label.
			// This is done in the mutex lock to avoid race conditions
			value.SwapState(currentState)
			value.Source = srcLabel
		}) // end busCache.DoMut()
	}

	// if the event is a notification then update the source label and add the deviationStartTime field to the transition time
	if (eventType == Notification) && (prevState == currentState) {
		busCache.DoMut(dstLabel, func(value *BusState) {
			value.Source = srcLabel

			// encase the state is primary and the transition time is nil, then use a hypen "-" to indicate no transition time.
			// Otherwise, get the transition time as a string
			event.PutValue("schedule.deviationStartTime", value.GetTransitionTimeStr(FlagOnlyTransition))
		})
	}

	// if the event is query then check if the state and manage it if it's in a defunct state.
	if eventType == Query {
		busCache.DoMut(dstLabel, func(value *BusState) {
			// recover the transition if in a defunct state.
			// must call CorrectDefunctTransition() 3x to allow time for a notification to properly update the state
			// this helps with top of the hour where a query may be processed before a notification
			// After 3x calls:
			// 1: if the state is Primary, then reset it calls ResetTransition() to clear the transition time
			// 2: if the state is not Primary, then it will set the transition time to now
			// 3: update the state and the srclabel to the current state and source label
			// 4: notify downstream systems of the corrected state
			var deviationStart string

			if currentState != prevState {
				logp.Warn("Busname: %s has a defunct transition previous: %s, current: %s", dstLabel, prevState, currentState)

				// when the stored state began and when the bus first left primary, before the correction moves or clears the transition times
				prevTime := value.GetTransitionTimeStr()
				firstDeviation := value.GetTransitionTimeStr(FlagTransitionStart, FlagOnlyTransition)

				if reset := value.CorrectDefunctTransition(currentState); reset {
					notifications = append(notifications, transitionNotifications(stateTransition{
						busname:    dstLabel,
						eventType:  eventType,
						trigger:    "GraphQL Query (Defunct Correction)",
						tags:       edge.Tags,
						prevState:  value.State,
						prevSource: value.Source,
						prevStart:  prevTime,
						state:      currentState,
						source:     srcLabel,
						time:       value.GetTransitionTimeStr(),
					})...)

					// a correction to primary ends the deviation now, report the window from when the bus first left primary
					if currentState == Primary {
						deviationStart = firstDeviation
						event.PutValue("schedule.deviationEndTime", time.Now().Format(time.RFC3339))
					}

					value.SwapState(currentState)
					value.Source = srcLabel

					logp.Warn("Busname: %s has been corrected to current state: %s", dstLabel, currentState)
				}
			}

			if deviationStart == "" {
				deviationStart = value.GetTransitionTimeStr(FlagOnlyTransition)
			}

			event.PutValue("schedule.deviationStartTime", deviationStart)
		})
	}

	// increment counter for the routing state
	counters.Increment(currentState)

	return &EventResults{event: &event, notifications: notifications}, nil
}
