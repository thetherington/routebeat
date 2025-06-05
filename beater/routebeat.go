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

	"github.com/thetherington/routebeat/beater/httpclient"
	routeCfg "github.com/thetherington/routebeat/config"
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

	// create the graphql query client
	queryClient := graphql.NewClient(bt.config.API.Url, bt.httpClient)

	// run the query client in a seperate go routine for each tag
	for _, tag := range bt.config.Tags {
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

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := client.Query(ctx, &query, variables); err != nil {
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
		events        []beat.Event
		processed     int
		discarded     int
		staticNameset bool = bt.config.Nameset.Value != ""
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
				"id":                edge.Id,
				"name":              edge.Name,
				"dstType":           edge.Type,
				"isSub":             edge.IsSub,
				"isDst":             edge.IsDst,
				"type":              eventType.String(),
				"tags":              edge.Tags,
				"tag":               tag,
				"nameset":           mapstr.M{},
				"routeableTerminal": mapstr.M{},
			},
		}

		// update the event fields on whether the source configuration exists
		if staticNameset {
			event.PutValue("source", bt.config.Nameset.Default)
			event.PutValue("destination", findNamesetValueByName(bt.config.Nameset.Value, edge.NamesetNames, bt.config.Nameset.Default))
		}

		// put in the nameset name and value "event.nameset.<nameset name>"
		for _, n := range edge.NamesetNames {
			event.PutValue(
				fmt.Sprintf("nameset.%s", strings.ToLower(n.Nameset.Name)),
				n.Name,
			)
		}

		// destination has not physical source or subscribed source
		if edge.RoutedPhysicalSource == nil && edge.SubscribedSource == nil {
			event.Delete("routeableTerminal")

			// switch eventType {
			// case Query:

			// case Notification:
			// 	discarded++
			// 	continue
			// }
		}

		// create a nested object for the RoutePhysicalSource
		if edge.RoutedPhysicalSource != nil {
			m := mapstr.M{
				"name":  edge.RoutedPhysicalSource.Name,
				"isSrc": edge.RoutedPhysicalSource.IsSrc,
			}

			rangeOverNamesets(edge.RoutedPhysicalSource.NamesetNames, &m)

			event.PutValue("routeableTerminal.physicalSource", m)

			if staticNameset {
				event.PutValue("source", findNamesetValueByName(
					bt.config.Nameset.Value,
					edge.RoutedPhysicalSource.NamesetNames,
					bt.config.Nameset.Default,
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

			if staticNameset {
				event.PutValue("source", findNamesetValueByName(
					bt.config.Nameset.Value,
					edge.SubscribedSource.NamesetNames,
					bt.config.Nameset.Default,
				))
			}
		}

		events = append(events, event)

		processed++
	}

	// if len(events) > 0 {
	// 	fmt.Printf("%+v\n", events[0])
	// }
	bt.client.PublishAll(events)

	logp.Debug("ProcessResults", "Tag: %s, Processed: %d, Discarded: %d, EventType: %s", tag, processed, discarded, eventType)
}
