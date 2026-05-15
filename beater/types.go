package beater

type EventType int

const (
	Query EventType = iota
	Notification
	Scan
)

var eventName = map[EventType]string{
	Query:        "query",
	Notification: "notification",
	Scan:         "scan",
}

func (et EventType) String() string {
	return eventName[et]
}

type SlabMap map[string]SlabPartial

type SlabPartial struct {
	Id     string
	Name   string
	Device string
	Output int
}
