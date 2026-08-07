package beater

import (
	"errors"
	"slices"
	"time"
)

var (
	ErrEdgeTagNotFound     = errors.New("tag not found in edge tags list")
	ErrScheduleBusNotFound = errors.New("buscode not found in schedule busmap cache")
	ErrScheduleBusExpired  = errors.New("buscode in schedule cache end date expired")
)

// EventType is a small enum
type EventType int

const (
	Query EventType = iota
	Notification
	Summary
)

var eventName = map[EventType]string{
	Query:        "query",
	Notification: "notification",
	Summary:      "summary",
}

func (et EventType) String() string {
	return eventName[et]
}

// RoutingState is a small enum
type RoutingState int

const (
	Unknown RoutingState = iota
	Primary
	Backup
	Zorro
	TDA
	Unsched
)

var routingName = map[RoutingState]string{
	Primary: "Primary",
	Backup:  "Backup",
	Zorro:   "Zorro",
	TDA:     "TDA",
	Unsched: "UnscheduledAudio",
	Unknown: "unknown",
}

func (rs RoutingState) String() string {
	return routingName[rs]
}

// Summary struct
type Counters struct {
	Tag     string
	Primary RoutingState
	Backup  RoutingState
	Zorro   RoutingState
	Tda     RoutingState
	Unsched RoutingState
}

func (s *Counters) Increment(field RoutingState) {
	switch field {
	case Primary:
		s.Primary++
	case Backup:
		s.Backup++
	case Zorro:
		s.Zorro++
	case TDA:
		s.Tda++
	case Unsched:
		s.Unsched++
	}
}

// Blend merges the values from another Counter into the current one.
func (c *Counters) Merge(value *Counters) {
	c.Primary += value.Primary
	c.Backup += value.Backup
	c.Zorro += value.Zorro
	c.Tda += value.Tda
	c.Unsched += value.Unsched
}

func (s *Counters) Decrement(field RoutingState) {
	switch field {
	case Primary:
		s.Primary--
	case Backup:
		s.Backup--
	case Zorro:
		s.Zorro--
	case TDA:
		s.Tda--
	case Unsched:
		s.Unsched--
	}
}

type BusState struct {
	State           RoutingState
	Source          string
	Transition      *time.Time
	TransitionStart *time.Time
	Restore         *time.Time
	Counter         int
}

func NewBusState(state RoutingState, source string) *BusState {
	return &BusState{State: state, Source: source}
}

// SwapState replaces the State and returns the old state
func (bs *BusState) SwapState(rs RoutingState) RoutingState {
	temp := bs.State
	bs.State = rs

	return temp
}

type OptionalFlag int

const (
	FlagOnlyTransition OptionalFlag = iota
	FlagPreviousTime
	FlagTransitionStart
)

// Set the transition time.  if the optional flag FlagPreviousTime then return the previous transition time string instead of the new one
func (bs *BusState) SetTransitionTime(t time.Time, flag ...OptionalFlag) string {
	defer func() {
		// if the restore time is set, then set the transition begin time to the transition time
		if bs.Restore != nil {
			bs.TransitionStart = &t
		}

		bs.Restore = nil
	}()

	if len(flag) > 0 && slices.Contains(flag, FlagPreviousTime) {
		x := bs.GetTransitionTimeStr()
		bs.Transition = &t

		return x
	}

	bs.Transition = &t
	return bs.GetTransitionTimeStr()
}

// get the RFC3339 time format.  if the transition is nil then the return value is "-"
// flag FlagOnlyTransition can be used to indicate only Transition Time or return "-"
func (bs *BusState) GetTransitionTimeStr(flag ...OptionalFlag) string {
	// if either the transition or restore is nil, just return "-" regardless
	if bs.Transition == nil && bs.Restore == nil {
		return "-"
	}

	// if the user sets the transitionBegin argument to true, then return the transition begin time or "-" if nil
	if len(flag) > 0 && slices.Contains(flag, FlagTransitionStart) {
		if bs.TransitionStart == nil {
			if slices.Contains(flag, FlagOnlyTransition) && bs.Transition != nil {
				return bs.Transition.Format(time.RFC3339)
			}

			return "-"
		}

		return bs.TransitionStart.Format(time.RFC3339)
	}

	// if the user sets the onlyTransitionTime argument to true, then return only the transition time or "-" if nil
	if len(flag) > 0 && slices.Contains(flag, FlagOnlyTransition) {
		if bs.Transition == nil {
			return "-"
		}

		return bs.Transition.Format(time.RFC3339)
	}

	// if the transition is nil and the restore value is not nil, then use the restore
	if bs.Transition == nil && bs.Restore != nil {
		return bs.Restore.Format(time.RFC3339)
	}

	return bs.Transition.Format(time.RFC3339)
}

// Set the transition to be nil for the next transition. return the time now
// TODO return the duration would be better
func (bs *BusState) ResetTransition() string {
	t := time.Now()

	bs.Restore = &t
	bs.Transition = nil
	bs.TransitionStart = nil
	bs.Counter = 0

	return t.Format(time.RFC3339)
}

// checks whether the transition is in a defunct state if the new state is Primary and the stored state is not Primary,
// then the transition is defunct.  This is used to self heal the transition state
func (bs *BusState) IsDefunctTransition(newState RoutingState) bool {
	return newState != bs.State
}

// call this 3x times to heal the transition defunct state
func (bs *BusState) CorrectDefunctTransition(state RoutingState) bool {
	if bs.Counter > 2 {
		if state == Primary {
			bs.ResetTransition()
		} else {
			bs.SetTransitionTime(time.Now())
		}

		return true
	}

	bs.Counter++
	return false
}
