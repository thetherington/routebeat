package beater

import (
	"errors"
	"fmt"
	"slices"
	"time"

	insite "github.com/thetherington/routebeat/beater/analytics"
)

const schedulerDateLayout = "2006/01/02 15:04:05"

// schedulerTask is a minimal view of a scheduler event used to check the task type
// without decoding the full event (other event shapes may not fit SchedulerAPGEvent)
type schedulerTask struct {
	Task string `json:"task"`
}

type SchedulerAPGEvent struct {
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

var (
	errEventInactive = errors.New("scheduler event is not active")
	errEventNoTag    = errors.New("scheduler event has no MAIN or BACKUP tag")
)

// busRouting converts the event into a bus routing object if the event is active at now
// and is tagged MAIN or BACKUP. returns an error if the event should be discarded
func (e *SchedulerAPGEvent) busRouting(now time.Time) (*insite.BusRouting, error) {
	startTime, err := time.Parse(schedulerDateLayout, e.StartDate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse start date: %w", err)
	}

	endTime, err := time.Parse(schedulerDateLayout, e.EndDate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse end date: %w", err)
	}

	if !isActive(now, &startTime, &endTime) {
		return nil, errEventInactive
	}

	br := &insite.BusRouting{
		StartDate: &startTime,
		EndDate:   &endTime,
	}

	// determine if the event is a primary or secondary bus routing based on the tags
	switch {
	case slices.Contains(e.Tags, "MAIN"):
		br.Pri = e.Input
	case slices.Contains(e.Tags, "BACKUP"):
		br.Sec = e.Input
	default:
		return nil, errEventNoTag
	}

	return br, nil
}

/**
webhook / events api payload. start and end are strings here, they're converted to numbers
when the scheduler events are ingested into elasticsearch
{
        "end_date": "2026/08/16 14:00:00",
        "output_type": "magnum",
        "output_id": "46e42e0d-c90b-55e0-a3b6-c4eff685516e",
        "event_id": "88a3dccc-0725-4e15-a52f-8694e6675abe",
        "error": "",
        "history": [],
        "start": "1786885320000000000",
        "start_date": "2026/08/16 13:02:00",
        "output_pooled": false,
        "schedule_id": "12856216&1786885320000000000",
        "entry": "manual",
        "state": 1,
        "input_blocking": false,
        "input_pooled": false,
        "task": "apg",
        "input_id": "c8348ef8-f852-537f-90fa-60e75da8dee3",
        "end_offset": "0",
        "input_type": "magnum",
        "input": "SILENCE",
        "event_params": {
          "apg_id": "36857574",
          "pri_src": "{event.input}",
          "sec_src": "SILENCE",
          "historic": true,
          "bus_name": "{event.output}",
          "apg_rule": "NOOP"
        },
        "recur": {},
        "output_blocking": true,
        "comment": "",
        "tags": [
          "MAIN"
        ],
        "end": "1786888800000000000",
        "start_offset": "0",
        "output": "TLUPDT"
      }
	**/
