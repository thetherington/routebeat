package schedule

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// ErrNoTag is returned for an apg event that isn't tagged MAIN or BACKUP
var ErrNoTag = errors.New("scheduler event has no MAIN or BACKUP tag")

// quoted task value used to cheaply discard non-apg events before any json decoding
var apgTaskBytes = []byte(`"apg"`)

// taskView is a minimal view of a scheduler event used to check the task type
// without decoding the full event (other event shapes may not fit Event)
type taskView struct {
	Task string `json:"task"`
}

// Event is an apg scheduler event as returned by the events api.
// start and end are strings here, they're converted to numbers when ingested into elasticsearch
type Event struct {
	EndDate       string `json:"end_date"`
	OutputType    string `json:"output_type"`
	OutputID      string `json:"output_id"`
	EventID       string `json:"event_id"`
	Error         string `json:"error"`
	History       []any  `json:"history"`
	Start         string `json:"start"`
	StartDate     string `json:"start_date"`
	OutputPooled  bool   `json:"output_pooled"`
	ScheduleID    string `json:"schedule_id"`
	Entry         string `json:"entry"`
	State         int    `json:"state"`
	InputBlocking bool   `json:"input_blocking"`
	InputPooled   bool   `json:"input_pooled"`
	Task          string `json:"task"`
	InputID       string `json:"input_id"`
	EndOffset     string `json:"end_offset"`
	InputType     string `json:"input_type"`
	Input         string `json:"input"`
	EventParams   struct {
		ApgID    string `json:"apg_id"`
		PriSrc   string `json:"pri_src"`
		SecSrc   string `json:"sec_src"`
		Historic bool   `json:"historic"`
		BusName  string `json:"bus_name"`
		ApgRule  string `json:"apg_rule"`
	} `json:"event_params"`
	Recur struct {
	} `json:"recur"`
	OutputBlocking bool     `json:"output_blocking"`
	Comment        string   `json:"comment"`
	Tags           []string `json:"tags"`
	End            string   `json:"end"`
	StartOffset    string   `json:"start_offset"`
	Output         string   `json:"output"`
}

// Stats counts what happened to the items of a payload while parsing and building routings
type Stats struct {
	Events   int // apg events decoded
	NonApg   int // items dropped as another task or shape
	Invalid  int // items that failed to decode, or apg events with a bad date
	NoTag    int // apg events without a MAIN or BACKUP tag
	Routings int // routing windows built
}

// Add sums the counts of o into s
func (s *Stats) Add(o Stats) {
	s.Events += o.Events
	s.NonApg += o.NonApg
	s.Invalid += o.Invalid
	s.NoTag += o.NoTag
	s.Routings += o.Routings
}

// Routing converts the event into a routing window: the start/end dates and the input as the
// primary (MAIN tag) or the backup (BACKUP tag). returns ErrNoTag for any other tag
func (e *Event) Routing() (*BusRouting, error) {
	start, err := ParseDate(e.StartDate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse start date: %w", err)
	}

	end, err := ParseDate(e.EndDate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse end date: %w", err)
	}

	br := &BusRouting{StartDate: start, EndDate: end}

	// determine if the event is a primary or secondary bus routing based on the tags
	switch {
	case slices.Contains(e.Tags, "MAIN"):
		br.Pri = e.Input
	case slices.Contains(e.Tags, "BACKUP"):
		br.Sec = e.Input
	default:
		return nil, ErrNoTag
	}

	return br, nil
}

// ParseEvents decodes a payload (a JSON array of events, or a single event object) into apg events.
// Items that are not apg or fail to decode are counted and skipped. Only a payload that is not JSON,
// or is malformed JSON, returns an error
func ParseEvents(payload []byte) ([]Event, Stats, error) {
	var (
		items []json.RawMessage
		stats Stats
	)

	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil, stats, fmt.Errorf("empty payload after trimming")
	}

	switch trimmed[0] {
	case '{':
		if !json.Valid(trimmed) {
			return nil, stats, fmt.Errorf("malformed JSON object payload")
		}

		items = []json.RawMessage{trimmed}

	case '[':
		// unmarshal validates the whole array
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, stats, fmt.Errorf("malformed JSON array payload: %v", err)
		}

	default:
		if !json.Valid(trimmed) {
			return nil, stats, fmt.Errorf("payload is not JSON")
		}

		// valid JSON that is not an event (string, number, boolean or null)
		stats.NonApg++
		return nil, stats, nil
	}

	events := make([]Event, 0, len(items))

	for _, item := range items {
		// cheap pre-filter: discard items without an "apg" value before any json decoding
		if !bytes.Contains(item, apgTaskBytes) {
			stats.NonApg++
			continue
		}

		// decode only the task field to confirm the task type. other event shapes may not fit Event
		var task taskView
		if err := json.Unmarshal(item, &task); err != nil {
			stats.Invalid++
			continue
		}

		if task.Task != "apg" {
			stats.NonApg++
			continue
		}

		var event Event
		if err := json.Unmarshal(item, &event); err != nil {
			stats.Invalid++
			continue
		}

		events = append(events, event)
	}

	stats.Events = len(events)

	return events, stats, nil
}
