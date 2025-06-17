package beater

import "errors"

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
	State RoutingState
}

// SwapState replaces the State and returns the old state
func (bs *BusState) SwapState(rs RoutingState) RoutingState {
	temp := bs.State
	bs.State = rs
	return temp
}
