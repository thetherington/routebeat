package beater

import (
	"errors"
	"time"
)

var (
	ErrEdgeTagNotFound     = errors.New("tag not found in edge tags list")
	ErrScheduleBusNotFound = errors.New("buscode not found in schedule busmap cache")
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
	State      RoutingState
	Transition *time.Time
	Counter    int
}

func NewBusState(state RoutingState) *BusState {
	return &BusState{State: state}
}

// SwapState replaces the State and returns the old state
func (bs *BusState) SwapState(rs RoutingState) RoutingState {
	temp := bs.State
	bs.State = rs

	return temp
}

func (bs *BusState) SetTransitionTime(t time.Time) string {
	bs.Transition = &t
	return bs.GetTransitionTimeStr()
}

// get the RFC3339 time format.  if the transition is nil then the return value is "-"
func (bs *BusState) GetTransitionTimeStr() string {
	if bs.Transition == nil {
		return "-"
	}

	return bs.Transition.Format(time.RFC3339)
}

// Set the transition to be nil for the next transition. return the time now
// TODO return the duration would be better
func (bs *BusState) ResetTransition() string {
	bs.Transition = nil
	bs.Counter = 0
	return time.Now().Format(time.RFC3339)
}

// checks whether the transition is in a defunct state if the state is Primary and the transition time is set
func (bs *BusState) IsDefunctTransition() bool {
	return bs.State == Primary && bs.Transition != nil
}

// call this 3x times to heal the transition defunct state
func (bs *BusState) CorrectDefunctTransition() {
	if bs.Counter > 2 {
		bs.ResetTransition()
		return
	}

	bs.Counter++
}
