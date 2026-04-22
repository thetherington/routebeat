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

type EventType int

const (
	Query EventType = iota
	Notification
)

var eventName = map[EventType]string{
	Query:        "query",
	Notification: "notification",
}

func (et EventType) String() string {
	return eventName[et]
}

var (
	db        analytics.SearchInterface
	logCache  *cache.Cache
	scheduler gocron.Scheduler
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

	// Initialize the log cache for 40 minutes
	logCache = cache.NewCache(40 * time.Minute)

	// Initialize the scheduler
	sched, _ := gocron.NewScheduler()
	scheduler = sched

	scheduler.Start()

	// Validate there is atleast 1 tag
	if len(c.Tags) < 1 {
		return nil, errors.New("beat requires atleast 1 tag in the configuration")
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
			Address: c.ES.Address,
			Index:   c.ES.Index,
			Strict:  c.ES.Strict,
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

	// Disable GraphQL query client for now (only need subscriptions)
	// create the graphql query client
	// queryClient := graphql.NewClient(bt.config.API.Url, bt.httpClient)

	// run the query client in a seperate go routine for each tag
	// for _, tag := range bt.config.Tags {
	// go bt.QueryTerminalsRoutine(queryClient, tag, bt.done)
	// }

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

func (bt *routebeat) QueryTerminalsRoutine(client *graphql.Client, tag string, done chan struct{}) {
	// variables
	variables := map[string]any{
		"tag":   tag,
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
			ctx, cancel := context.WithTimeout(context.Background(), CLIENT_TIMEOUT*time.Second)
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
		"tag": tag,
	}

	// subscribe to a query and run a callback function to process the messages
	id, err := bt.subClient.Subscribe(query, v, func(message []byte, err error) error {
		if err != nil {
			return err
		}

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

// Builds beat events based on the Edge{} struct definition
func (bt *routebeat) BuildEvents(tag string, edges []Edge, eventType EventType) {
	logp.Debug("ProcessResults", "Query Results for Tag: %s, Total Edges:%d, EventType %s", tag, len(edges), eventType)

	var (
		events    []beat.Event
		buscodes  []string
		processed int
		discarded int
		mapping   bool = bt.config.Mapping != nil
	)

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

	// if there is an elasticsearch client and there are events to process
	// then schedule a job to analyze the logs for these events and publish them after the configured delay
	if db != nil && len(events) > 0 {
		_, err := scheduler.NewJob(
			gocron.OneTimeJob(
				gocron.OneTimeJobStartDateTime(time.Now().Add(bt.config.ES.Delay)),
			),
			gocron.NewTask(
				func() {
					for i, event := range events {
						// extract srcId and dstId from the event for log analysis
						srcId, dstId, err := getSrcDstIds(&event)
						if err != nil {
							logp.Err("Failed to get srcId/dstId from event: %v, error: %s", event, err.Error())
							continue
						}

						// analyze the logs for this event and enrich the event with log data if there is a match
						if err := bt.AnalyzeLogCollection(srcId, dstId, &event); err != nil {
							HandleAnalyzeLogError(err, srcId, dstId, &event)
						}

						events[i] = event
					}

					bt.client.PublishAll(events)
				},
			),
		)
		if err != nil {
			logp.Err("Failed to create scheduler job for events #%d:%v", len(events), err)
		}

		logp.Debug("ProcessResults", "Scheduled %d events for Tag: %s with for: [%v], EventType: %s", len(events), tag, strings.Join(buscodes, ","), eventType)
	}

	logp.Debug("ProcessResults", "Tag: %s, Processed: %d, Discarded: %d, EventType: %s for: [%v]", tag, processed, discarded, eventType, strings.Join(buscodes, ","))
}

func (bt *routebeat) AnalyzeLogCollection(src, dst string, event *beat.Event) error {
	if src == "" || dst == "" {
		return fmt.Errorf("source or destination is blank")
	}

	var swept bool = false

	logs, err := db.SearchLogs(src, dst)
	if err != nil {
		if err == analytics.ErrNoResults {
			return ErrNoLogsFound
		}

		return fmt.Errorf("failed to find logs for event: %v", err)
	}

	var (
		request  analytics.Source
		complete analytics.Source
	)

	// check if any of the logs contain the route subscribe request
	for _, log := range logs {
		if strings.Contains(log.Log.Syslog.Message, analytics.REQUEST_LOGS) {
			// compare the timestamp of the request log to the event timestamp to ensure it's within a 10 second window
			if (absDuration(event.Timestamp.Sub(log.Device.Timestamp)) <= bt.config.ES.Window) && !logCache.Exists(log.Id) {
				request = log
				break
			}
		}
	}

	// No request log was found with the configured window and if sweeping is enabled,
	// then we can look for a request log that is outside the window but within the last 20 minutes and use that as the request log.
	// This is to handle cases where this is a non route notification and there are syslog message with no matched notifications.
	if request.Device.Timestamp.IsZero() && bt.config.ES.Sweeping {
		for _, log := range logs {
			if strings.Contains(log.Log.Syslog.Message, analytics.REQUEST_LOGS) {
				if !logCache.Exists(log.Id) {
					request = log
					swept = true
					break
				}
			}
		}
	}

	if request.Device.Timestamp.IsZero() {
		return ErrNoRequestLogs
	}

	// check if any of the logs contain the route subscribe complete
	for i, log := range logs {
		if strings.Contains(log.Log.Syslog.Message, analytics.COMPLETION_LOGS) {
			// compare the timestamp of the complete log that it's greater than the request time and the log is not in cache.
			if log.Device.Timestamp.After(request.Device.Timestamp) && !logCache.Exists(log.Id) {
				// check that if the previous log is a request log and is the matched request log
				// ensures that a request log isn't orphaned without a completion log. If so, then mark the request log id in the cache to prevent future matches and return no completed logs error to prevent publishing an event with just a request log and no completion log.
				if i > 0 && strings.Contains(logs[i-1].Log.Syslog.Message, analytics.REQUEST_LOGS) && logs[i-1].Id != request.Id {
					logp.Err("Orphaned request log found for log id: %v (previous id: %v), source: %s, destination: %s, swept: %v", request.Id, logs[i-1].Id, src, dst, swept)
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
		return ErrNoCompletedLogs
	}

	// Mark these ids as processed in the cache
	if request.Id != "" {
		logCache.Add(request.Id)
	}
	if complete.Id != "" {
		// check if the complete log is a salvo log that contains multiple ids of other request logs. if so don't add it to the cache.
		// if the number of occurences of "sub_dst" in the complete log message is greater than 1, then it's a salvo log and we shouldn't add it to the cache
		// because it could be matched to multiple request logs. This is a heuristic that may need to be adjusted based on the actual log messages.
		if strings.Count(complete.Log.Syslog.Message, "sub_dst") <= 1 {
			logCache.Add(complete.Id)
		}
	}

	// calculate the duration between the request and complete logs
	event.PutValue("durationMs", complete.Device.Timestamp.Sub(request.Device.Timestamp).Milliseconds())

	// add the request and complete log information to the event
	event.PutValue("logs", mapstr.M{
		"request": mapstr.M{
			"time":    request.Device.Timestamp,
			"message": request.Log.Syslog.Message,
		},
		"complete": mapstr.M{
			"time":    complete.Device.Timestamp,
			"message": complete.Log.Syslog.Message,
		},
	})

	event.PutValue("matched", true)

	event.PutValue("swept", swept)

	// replace the event timestamp with the request log timestamp
	event.Timestamp = request.Device.Timestamp

	return nil
}
