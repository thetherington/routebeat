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
	"github.com/hasura/go-graphql-client"
	"github.com/hasura/go-graphql-client/pkg/jsonutil"

	insite "github.com/thetherington/routebeat/beater/analytics"
	"github.com/thetherington/routebeat/beater/cache"
	"github.com/thetherington/routebeat/beater/httpclient"
	routeCfg "github.com/thetherington/routebeat/config"
)

const (
	CLIENT_TIMEOUT    = 10 // time in seconds
	ANALYTICS_TIMEOUT = 10
	BUSCACHE_FILE     = "busCache.gob"
)

var (
	db            insite.SearchInterface
	scheduleCache = cache.NewCacheMap[string, *insite.BusRouting]()
	countersCache = cache.NewCacheMap[string, *Counters]()
	busCache      = cache.NewCacheMap[string, *BusState]()
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

	// Validate there is atleast 1 tag
	if len(c.Tags) < 1 {
		return nil, errors.New("beat requires atleast 1 tag in the configuration")
	}

	// Validate if mapping is enabled then the nameset is not blank
	if c.Mapping != nil && c.Mapping.Nameset == "" {
		return nil, errors.New("nameset cannot be blank if mapping is enabled")
	}

	// Create the elasticsearch client handler
	var err error
	db, err = insite.NewClient(&insite.ClientConfig{
		Address: c.ES.Address,
		Index:   c.ES.Index,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create elasticsearch client: %w", err)
	}

	// try to load the busCache from file
	if err := busCache.LoadFromFile(BUSCACHE_FILE); err != nil {
		logp.Err("failed to load busCache from file: %v", err)
	} else {
		logp.Info("Loaded busCache length: %d keys", busCache.Length())
	}

	done := make(chan struct{})

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

	// start the elasticsearch query routine (5 minutes default)
	go func() {
		ticker := time.NewTicker(bt.config.ES.Period)

		for {
			bm, err := func() (map[string]*insite.BusRouting, error) {
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
			case <-bt.done:
				logp.Warn("exiting elasticsearch QueryScheduler() routine")
			case <-ticker.C:
			}
		}
	}()

	// go routine to save the busCache to file every 10 minutes incase of a crash
	go func() {
		ticker := time.NewTicker(10 * time.Minute)

		select {
		case <-bt.done:
			logp.Warn("exiting busCache save to file routine")
			busCache.SaveToFile(BUSCACHE_FILE)
		case <-ticker.C:
		}

		if err := busCache.SaveToFile(BUSCACHE_FILE); err != nil {
			logp.Err("failed to save busCache to file: %v", err)
		}
	}()

	// create the graphql query client
	queryClient := graphql.NewClient(bt.config.API.Url, bt.httpClient)

	// run the query client in a seperate go routine for each tag
	for _, tag := range bt.config.Tags {
		// set counters cache for query
		countersCache.Set(tag, &Counters{})

		go bt.QueryTerminalsRoutine(queryClient, tag, bt.done)
	}

	// create the subscription client whether it's needed or not
	bt.subClient = graphql.NewSubscriptionClient(getWssURL(bt.config.API.Url)).
		WithWebSocketOptions(graphql.WebsocketOptions{
			HTTPClient: bt.httpClient,
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

		// start the subscriptions in the background
		go bt.subClient.Run()
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
	//   - elasticsearch scheduleQuery
	//   - busCache.SaveToFile
	//   - query terminals routine
	//   - exits beat run function
	close(bt.done)
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

			if err := client.Query(ctx, &query, variables); err != nil {
				return fmt.Errorf("error query failed for Tag: %s: %v", tag, err)
			}

			return nil
		}()
		if err != nil {
			logp.Err(err.Error())
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
			logp.Err("error making subscription query or callback %v", err)
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
		events []beat.Event

		processed int // counter for any routes with matched exact tag
		discarded int // counter for any routes with unmatched exact tag
		unmatched int // counter for any destinations not in the cache

		counters = &Counters{Tag: tag} // counters for the schedule state routing
	)

	for _, edge := range edges {
		// create beat event from edge information and update counters on route deviation findings
		event, err := bt.CreateEventFromEdge(&edge, tag, counters, eventType)
		if err != nil {
			if errors.Is(err, ErrEdgeTagNotFound) {
				discarded++
				continue
			}

			if errors.Is(err, ErrScheduleBusNotFound) {
				unmatched++
			}
		}

		events = append(events, *event)
		processed++
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
				"eventType":      Summary.String(),
				"unmatched":      unmatched,
				"processed":      processed,
				"summaryTag":     c.Tag,
				Primary.String(): c.Primary,
				Backup.String():  c.Backup,
				Zorro.String():   c.Zorro,
				TDA.String():     c.Tda,
				Unsched.String(): c.Unsched,
			},
		})
	}

	bt.client.PublishAll(events)

	logp.Debug("ProcessResults", "Tag: %s, Processed: %d, Discarded: %d, Unmatched: %d EventType: %s", tag, processed, discarded, unmatched, eventType)
}

func (bt *routebeat) CreateEventFromEdge(edge *Edge, tag string, counters *Counters, eventType EventType) (*beat.Event, error) {
	var (
		mapping bool = bt.config.Mapping != nil

		dstLabel     string       // broadview destination label
		srcLabel     string       // broadview source label
		prevState    RoutingState // placeholder to store the previous routing state (default Unknown)
		currentState RoutingState // placeholder to store the current routing state (default Unknown)
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
	}

	// destination has no physical source or subscribed source then remove the key
	if edge.RoutedPhysicalSource == nil && edge.SubscribedSource == nil {
		event.Delete("routeableTerminal")
	}

	// get the busrouting information from the schedule cache
	// return the event as-isif the cache doesn't have a key
	routing, ok := scheduleCache.Get(dstLabel)
	if !ok {
		event.PutValue("schedule.matched", false)
		return &event, ErrScheduleBusNotFound
	}

	fmt.Printf("\nDestination:%s Source:%s\n%+v\n", dstLabel, srcLabel, routing)

	switch srcLabel {
	case routing.Pri:
		currentState = Primary

	case routing.Sec:
		currentState = Backup

	case bt.config.Zorro:
		currentState = Zorro

	case bt.config.TDA:
		currentState = TDA

	default:
		currentState = Unsched
	}

	// schedule grouped data
	event.PutValue("schedule", mapstr.M{
		"buscode": dstLabel,
		"status":  currentState.String(),
		"primary": routing.Pri,
		"backup":  routing.Sec,
		"matched": true,
	})

	// increment counter for the routing state
	counters.Increment(currentState)

	// update the busState cache if the bus code doesn't exist
	// otherwise swap the current state and save the previous state
	busCache.Do(func(c *cache.CacheMap[string, *BusState]) {
		if v, ok := c.Store[dstLabel]; !ok {
			c.Store[dstLabel] = &BusState{State: currentState}
		} else {
			prevState = v.SwapState(currentState)
		}
	})

	// only create a negative counter value for Notification events
	// and if the state has actually changed
	if (eventType == Notification) && (prevState != currentState) {
		counters.Decrement(prevState)
	}

	return &event, nil
}
