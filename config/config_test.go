//go:build !integration
// +build !integration

package config

import (
	"testing"
	"time"

	"github.com/elastic/elastic-agent-libs/config"
)

func TestScheduleAndEventsAPIDefaults(t *testing.T) {
	c := DefaultConfig

	if err := config.MustNewConfigFrom(map[string]any{}).Unpack(&c); err != nil {
		t.Fatal(err)
	}

	if c.Schedule.Period != 5*time.Minute || c.Schedule.Lookahead != 25*time.Minute ||
		c.Schedule.StartTolerance != 5*time.Second || c.Schedule.Dev.LoadCache || !c.Schedule.Dev.ValidateEndDate {
		t.Errorf("schedule defaults = %+v", c.Schedule)
	}

	want := EventsAPI{
		Timeout:     3 * time.Second,
		BulkTimeout: 15 * time.Second,
		StartParam:  "start",
		EndParam:    "end",
		OutputParam: "output",
		TaskParam:   "task",
		Task:        "apg",
	}

	got := c.EventsAPI
	if got.Url != "" || got.Delay != 0 || len(got.Headers) != 0 || got.Timeout != want.Timeout ||
		got.BulkTimeout != want.BulkTimeout || got.StartParam != want.StartParam || got.EndParam != want.EndParam ||
		got.OutputParam != want.OutputParam || got.TaskParam != want.TaskParam || got.Task != want.Task {
		t.Errorf("events api defaults = %+v", got)
	}
}

func TestScheduleAndEventsAPIUnpack(t *testing.T) {
	c := DefaultConfig

	raw := config.MustNewConfigFrom(map[string]any{
		"schedule": map[string]any{
			"period":          "1m",
			"lookahead":       "30m",
			"start_tolerance": "0s",
			"dev":             map[string]any{"validate_end_date": false},
		},
		"events_api": map[string]any{
			"url":          "http://host/api/v1/scheduler/events",
			"delay":        "500ms",
			"bulk_timeout": "20s",
			"start_param":  "start_date",
			"task":         "",
			"headers":      map[string]any{"Authorization": "Bearer x"},
		},
		// removed blocks in an old config don't break the unpack
		"elasticsearch": map[string]any{"address": "http://127.0.0.1:9200", "period": "5m"},
		"webhook":       map[string]any{"enabled": true, "port": 8082},
	})

	if err := raw.Unpack(&c); err != nil {
		t.Fatal(err)
	}

	if c.Schedule.Period != time.Minute || c.Schedule.Lookahead != 30*time.Minute ||
		c.Schedule.StartTolerance != 0 || c.Schedule.Dev.ValidateEndDate {
		t.Errorf("schedule = %+v", c.Schedule)
	}

	e := c.EventsAPI
	if e.Url != "http://host/api/v1/scheduler/events" || e.Delay != 500*time.Millisecond || e.BulkTimeout != 20*time.Second ||
		e.Timeout != 3*time.Second || e.StartParam != "start_date" || e.EndParam != "end" || e.Task != "" ||
		e.Headers["Authorization"] != "Bearer x" {
		t.Errorf("events api = %+v", e)
	}
}
