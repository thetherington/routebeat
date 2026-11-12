package schedule

import (
	"slices"
	"testing"
	"time"
)

// fixed reference time
var testNow = time.Date(2026, 8, 16, 13, 30, 0, 0, time.UTC)

func timePtr(t time.Time) *time.Time { return &t }

func routing(pri, sec string, start, end time.Time) *BusRouting {
	return &BusRouting{Pri: pri, Sec: sec, StartDate: timePtr(start), EndDate: timePtr(end)}
}

func event(output, input, tag, start, end string) Event {
	return Event{Task: "apg", Output: output, Input: input, Tags: []string{tag}, StartDate: start, EndDate: end}
}

func TestMergeBusRouting(t *testing.T) {
	start, end := testNow.Add(-30*time.Minute), testNow.Add(30*time.Minute)

	t.Run("nil existing returns a copy", func(t *testing.T) {
		incoming := routing("SRC1", "", start, end)

		got := MergeBusRouting(nil, incoming)
		if got == incoming || *got != *incoming {
			t.Errorf("got %+v, want a copy of %+v", got, incoming)
		}
	})

	t.Run("overlapping window merges", func(t *testing.T) {
		existing := routing("OLD", "SEC", start, end)
		incoming := routing("NEW", "", start.Add(time.Minute), end)

		got := MergeBusRouting(existing, incoming)
		if got.Pri != "NEW" || got.Sec != "SEC" || !got.StartDate.Equal(*incoming.StartDate) {
			t.Errorf("got %+v", got)
		}

		if existing.Pri != "OLD" || incoming.Sec != "" {
			t.Error("inputs were modified")
		}
	})

	t.Run("non-overlapping window replaces", func(t *testing.T) {
		existing := routing("OLD", "SEC", start.Add(-2*time.Hour), start.Add(-time.Hour))
		incoming := routing("NEW", "", start, end)

		got := MergeBusRouting(existing, incoming)
		if got.Pri != "NEW" || got.Sec != "" {
			t.Errorf("got %+v, want the previous window's Sec dropped", got)
		}
	})
}

func TestBuildRoutes(t *testing.T) {
	const (
		h13 = "2026/08/16 13:00:00"
		h14 = "2026/08/16 14:00:00"
		h15 = "2026/08/16 15:00:00"
	)

	events := []Event{
		// BUS1: the 14-15 window listed first, MAIN and BACKUP of the 13-14 window not adjacent
		event("BUS1", "NEXT_PRI", "MAIN", h14, h15),
		event("BUS1", "PRI", "MAIN", h13, h14),
		event("BUS2", "B_PRI", "MAIN", h13, h14),
		event("BUS1", "SEC", "BACKUP", h13, h14),
		// BUS3: backup only
		event("BUS3", "C_SEC", "BACKUP", h13, h15),
		// skipped
		event("BUS4", "X", "EXTERNAL", h13, h14),
		event("BUS5", "X", "MAIN", "tomorrow", h14),
	}

	routes, stats := BuildRoutes(events)

	if stats.NoTag != 1 || stats.Invalid != 1 || stats.Routings != 4 {
		t.Errorf("stats = %+v, want noTag=1 invalid=1 routings=4", stats)
	}

	if len(routes) != 3 {
		t.Fatalf("outputs = %d, want 3 (%v)", len(routes), routes)
	}

	bus1 := routes["BUS1"]
	if len(bus1) != 2 {
		t.Fatalf("BUS1 windows = %d, want 2 (back-to-back windows stay separate)", len(bus1))
	}

	if bus1[0].Pri != "PRI" || bus1[0].Sec != "SEC" || bus1[0].StartDate.Format(DateLayout) != h13 {
		t.Errorf("BUS1[0] = %+v, want the folded 13-14 window first", bus1[0])
	}

	if bus1[1].Pri != "NEXT_PRI" || bus1[1].Sec != "" {
		t.Errorf("BUS1[1] = %+v", bus1[1])
	}

	if r := routes["BUS3"]; len(r) != 1 || r[0].Pri != "" || r[0].Sec != "C_SEC" {
		t.Errorf("BUS3 = %+v", r)
	}
}

func TestBuildRoutesRealPair(t *testing.T) {
	f := loadSchedulerFixture(t)

	main, backup := decodeOne(t, f.ApgPair[0]), decodeOne(t, f.ApgPair[1])

	routes, stats := BuildRoutes([]Event{main, backup})

	r := routes[main.Output]
	if stats.Routings != 1 || len(r) != 1 || r[0].Pri != main.Input || r[0].Sec != backup.Input {
		t.Errorf("%s = %+v (stats %+v), want pri=%s sec=%s", main.Output, r, stats, main.Input, backup.Input)
	}
}

func TestMergeRouteLists(t *testing.T) {
	a := routing("A", "B", testNow.Add(-time.Hour), testNow.Add(time.Hour))
	next := routing("N", "", testNow.Add(time.Hour), testNow.Add(2*time.Hour))
	dup := routing("A", "B", testNow.Add(-time.Hour), testNow.Add(time.Hour))

	now := map[string][]*BusRouting{
		"BUS1": {a},
		"BUS2": {nil, {Pri: "NO_WINDOW"}},
	}
	ahead := map[string][]*BusRouting{
		"BUS1": {next, dup}, // a long event is returned by both the now and the lookahead fetch
		"BUS3": {next},
	}

	merged := MergeRouteLists(now, ahead)

	if got := merged["BUS1"]; len(got) != 2 || got[0].Pri != "A" || got[1].Pri != "N" {
		t.Errorf("BUS1 = %+v, want the current and next windows once each, sorted", got)
	}

	if _, ok := merged["BUS2"]; ok {
		t.Error("BUS2 has no valid window and should be dropped")
	}

	if got := merged["BUS3"]; len(got) != 1 || got[0] == next {
		t.Errorf("BUS3 = %+v, want a copy of the lookahead window", got)
	}

	if !slices.Equal(now["BUS1"], []*BusRouting{a}) || a.Pri != "A" {
		t.Error("inputs were modified")
	}
}
