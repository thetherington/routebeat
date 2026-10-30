package beater

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"
	insite "github.com/thetherington/routebeat/beater/analytics"
	"github.com/thetherington/routebeat/beater/notify"
)

// hotCacheEnabled reports whether anything can update the scheduleHotCache (webhook or events api)
func (bt *routebeat) hotCacheEnabled() bool {
	return bt.config.Webhook.Enabled || bt.eventsAPI != nil
}

// matchScheduledState returns Primary or Backup if the source label matches the routing, otherwise Unsched.
// blank labels never match
func matchScheduledState(srcLabel string, routing *insite.BusRouting) RoutingState {
	switch {
	case srcLabel == "":
		return Unsched

	case srcLabel == routing.Pri:
		return Primary

	case srcLabel == routing.Sec:
		return Backup
	}

	return Unsched
}

// resolveRoutingState maps the source to Primary/Backup by label, then Zorro/TDA by source tag, otherwise Unsched.
// a blank source label can't be resolved and returns Unknown
func (bt *routebeat) resolveRoutingState(srcLabel string, srcTags []string, routing *insite.BusRouting) RoutingState {
	if srcLabel == "" {
		return Unknown
	}

	if state := matchScheduledState(srcLabel, routing); state != Unsched {
		return state
	}

	switch {
	case slices.Contains(srcTags, bt.config.Zorro):
		return Zorro

	case slices.Contains(srcTags, bt.config.TDA):
		return TDA
	}

	return Unsched
}

// scheduleFields builds the "schedule" group of the event
func scheduleFields(buscode string, state RoutingState, routing *insite.BusRouting, source RoutingSource) mapstr.M {
	return mapstr.M{
		"buscode":       buscode,
		"status":        state.String(),
		"primary":       routing.Pri,
		"backup":        routing.Sec,
		"matched":       true,
		"routingSource": source.String(),
	}
}

// stateTransition describes a bus moving from the previous state to the current state
type stateTransition struct {
	busname    string
	eventType  EventType
	trigger    string // e.g. "GraphQL Subscription"
	tags       []string
	prevState  RoutingState
	prevSource string
	prevStart  string // when the previous state began
	state      RoutingState
	source     string
	time       string // when the transition happened, the end of the previous state and the start of the current
}

// transitionNotifications builds the notifications for a state change
//
//	Notification 1 - closes the previous ticket with the previous status and an end time (skipped if the previous state is Unknown)
//	Notification 2 - opens a new ticket with the current status (no end time)
func transitionNotifications(tr stateTransition) []notify.Notification {
	// set the message type of the notification based on the current state
	var msgType notify.MessageType = notify.RouteValidationError
	if tr.state == Primary {
		msgType = notify.RouteValidationCorrection
	}

	notifications := make([]notify.Notification, 0, 2)

	if tr.prevState != Unknown {
		notifications = append(notifications, notify.NewBuilder(notifier.GetOrigin()).
			WithMessageByType(msgType).
			AddDetails(msgType, notify.Details{
				Status:    tr.prevState.String(),
				Busname:   tr.busname,
				Source:    tr.prevSource,
				Start:     tr.prevStart,
				End:       tr.time,
				EventType: tr.eventType.String(),
				Trigger:   tr.trigger + " (Previous State)",
			}).
			WithTags("previous").
			Build(),
		)
	}

	notifications = append(notifications, notify.NewBuilder(notifier.GetOrigin()).
		WithMessageByType(msgType).
		AddDetails(msgType, notify.Details{
			Status:    tr.state.String(),
			Busname:   tr.busname,
			Source:    tr.source,
			Start:     tr.time,
			End:       "",
			EventType: tr.eventType.String(),
			Tags:      strings.Join(tr.tags, ","),
			Trigger:   tr.trigger + " (Current State)",
		}).
		WithTags("current").
		Build(),
	)

	return notifications
}

// isUnscheduledTransition reports a notification that moved a known bus into the Unsched state.
// there's a chance of an advance event or a FF, so the hot cache may have a newer routing
func isUnscheduledTransition(eventType EventType, prevState, currentState RoutingState) bool {
	return eventType == Notification &&
		currentState == Unsched &&
		prevState != currentState &&
		prevState != Unknown
}

// recheckHotRoute refreshes the hot cache from the events api and/or waits for webhook data, then
// re-evaluates the source against the hot route. returns the original Unsched state, routing and source
// if the hot cache has no matching routing
func (bt *routebeat) recheckHotRoute(dstLabel, srcLabel string, routing *insite.BusRouting, source RoutingSource) (RoutingState, *insite.BusRouting, RoutingSource) {
	now := time.Now()

	matchHotRoute := func() (RoutingState, *insite.BusRouting, bool) {
		r, ok := GetScheduleHotRoute(dstLabel, now)
		if !ok {
			return Unsched, nil, false
		}

		state := matchScheduledState(srcLabel, r)
		if state == Unsched {
			return Unsched, nil, false
		}

		logp.Debug("ScheduleHotCache", "Found routing for Destination:%s Source:%s %+v", dstLabel, srcLabel, r)

		return state, r, true
	}

	// fetch the latest scheduler events for the destination from the events api.
	// the response payload is applied to the hot cache by the SchedulerHookCallback
	if bt.eventsAPI != nil {
		if err := bt.eventsAPI.Refresh(context.Background(), dstLabel); err != nil {
			logp.Warn("events api refresh failed for Destination:%s: %v", dstLabel, err)
		}

		if state, r, ok := matchHotRoute(); ok {
			return state, r, SourceHotCache
		}
	}

	// wait to allow time for the hot cache to be updated by the webhook
	if bt.config.Webhook.Enabled && bt.config.Webhook.Wait > 0 {
		time.Sleep(bt.config.Webhook.Wait)

		if state, r, ok := matchHotRoute(); ok {
			return state, r, SourceHotCache
		}
	}

	return Unsched, routing, source
}
