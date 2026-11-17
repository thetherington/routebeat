package beater

import (
	"slices"
	"strings"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"
	"github.com/thetherington/routebeat/beater/notify"
	"github.com/thetherington/routebeat/beater/schedule"
)

// matchScheduledState returns Primary or Backup if the source label matches the routing, otherwise Unsched.
// blank labels never match
func matchScheduledState(srcLabel string, routing *schedule.BusRouting) RoutingState {
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
func (bt *routebeat) resolveRoutingState(srcLabel string, srcTags []string, routing *schedule.BusRouting) RoutingState {
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
func scheduleFields(buscode string, state RoutingState, routing *schedule.BusRouting, source RoutingSource) mapstr.M {
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
// there's a chance of an advance event or a FF, so the events api may have a newer routing
func isUnscheduledTransition(eventType EventType, prevState, currentState RoutingState) bool {
	return eventType == Notification &&
		currentState == Unsched &&
		prevState != currentState &&
		prevState != Unknown
}

// recheck miss reasons logged under the ScheduleRecheckMiss debug selector
const (
	recheckMissNoRouting = "no_active_routing"    // no active routing for the destination after the refresh
	recheckMissNoMatch   = "source_not_scheduled" // active routing, but the source is neither its primary nor backup
)

// recheckRoute refreshes the destination's schedule from the events api, then re-evaluates the source
// against the refreshed routing. returns the original Unsched state, routing and source if the refreshed
// routing doesn't match the source
func (bt *routebeat) recheckRoute(dstLabel, srcLabel string, routing *schedule.BusRouting, source RoutingSource) (RoutingState, *schedule.BusRouting, RoutingSource) {
	if bt.eventsAPI == nil {
		return Unsched, routing, source
	}

	// optional pause in case the api has latency receiving new scheduler data
	if bt.config.EventsAPI.Delay > 0 {
		time.Sleep(bt.config.EventsAPI.Delay)
	}

	if _, err := bt.refreshOutput(dstLabel); err != nil {
		logp.Warn("events api refresh failed for Destination:%s: %v", dstLabel, err)
	}

	r, ok := scheduleStore.Active(dstLabel, time.Now(), bt.config.Schedule.Dev.ValidateEndDate)
	if !ok {
		logp.Debug("ScheduleRecheckMiss", "Recheck miss Destination:%s Source:%s Reason:%s Primary: Backup:",
			dstLabel, srcLabel, recheckMissNoRouting)

		return Unsched, routing, source
	}

	state := matchScheduledState(srcLabel, r)
	if state == Unsched {
		logp.Debug("ScheduleRecheckMiss", "Recheck miss Destination:%s Source:%s Reason:%s Primary:%s Backup:%s",
			dstLabel, srcLabel, recheckMissNoMatch, r.Pri, r.Sec)

		return Unsched, routing, source
	}

	logp.Debug("ScheduleRecheck", "Recheck hit Destination:%s Source:%s State:%s Primary:%s Backup:%s",
		dstLabel, srcLabel, state, r.Pri, r.Sec)

	return state, r, SourceRecheck
}
