package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func apgEventJSON(output, input, tag, start, end string) string {
	return fmt.Sprintf(`{"task":"apg","output":%q,"input":%q,"tags":[%q],"start_date":%q,"end_date":%q,"state":1,"history":[]}`,
		output, input, tag, start, end)
}

func TestParseEvents(t *testing.T) {
	apg := apgEventJSON("TLUPDT", "SRC1", "MAIN", "2026/08/16 13:00:00", "2026/08/16 14:00:00")
	deleted := strings.Replace(apg, `"state":1`, `"state":7`, 1)

	tests := []struct {
		name        string
		payload     string
		wantErr     bool
		wantEvents  int
		wantNonApg  int
		wantInvalid int
		wantDeleted int
	}{
		{name: "deleted event is skipped", payload: deleted, wantDeleted: 1},
		{name: "deleted event in an array", payload: "[" + deleted + "," + apg + "]", wantEvents: 1, wantDeleted: 1},
		{name: "single object", payload: apg, wantEvents: 1},
		{name: "array", payload: "[" + apg + "," + apg + "]", wantEvents: 2},
		{name: "leading whitespace", payload: "\n\t " + apg, wantEvents: 1},
		{name: "non-apg object", payload: `{"task":"router","output":"X"}`, wantNonApg: 1},
		{name: "non-apg with a different shape", payload: `[{"task":"router","state":"running","history":{"a":1}}]`, wantNonApg: 1},
		{name: "apg value in another field", payload: `[{"task":"router","comment":"apg"}]`, wantNonApg: 1},
		{name: "task is not a string", payload: `[{"task":5,"comment":"apg"}]`, wantInvalid: 1},
		{name: "apg event that does not fit the struct", payload: `[{"task":"apg","state":"running"}]`, wantInvalid: 1},
		{
			name:       "mixed array keeps valid items",
			payload:    "[" + apg + `,{"task":"router"},{"task":"apg","state":"bad"},` + apg + "]",
			wantEvents: 2, wantNonApg: 1, wantInvalid: 1,
		},
		// valid JSON that isn't an event is skipped without an error
		{name: "json string", payload: `"apg"`, wantNonApg: 1},
		{name: "json number", payload: `42`, wantNonApg: 1},
		{name: "json null", payload: `null`, wantNonApg: 1},
		{name: "empty object", payload: `{}`, wantNonApg: 1},
		{name: "empty array", payload: `[]`},
		{name: "array of scalars", payload: `[1,"apg",null]`, wantNonApg: 2, wantInvalid: 1},

		// only payloads that aren't JSON, or are malformed JSON, are errors
		{name: "empty", payload: "   ", wantErr: true},
		{name: "not json", payload: `task=apg`, wantErr: true},
		{name: "malformed array", payload: `[{"task":"apg"`, wantErr: true},
		{name: "malformed object", payload: `{"task":"apg"`, wantErr: true},
		{name: "object with trailing data", payload: `{"task":"apg"} {"task":"apg"}`, wantErr: true},
		{name: "malformed item in an array", payload: `[{"task":"apg"},{"task":}]`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, stats, err := ParseEvents([]byte(tt.payload))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}

			if len(events) != tt.wantEvents || stats.Events != tt.wantEvents {
				t.Errorf("events = %d (stats %d), want %d", len(events), stats.Events, tt.wantEvents)
			}

			if stats.NonApg != tt.wantNonApg {
				t.Errorf("nonApg = %d, want %d", stats.NonApg, tt.wantNonApg)
			}

			if stats.Invalid != tt.wantInvalid {
				t.Errorf("invalid = %d, want %d", stats.Invalid, tt.wantInvalid)
			}

			if stats.Deleted != tt.wantDeleted {
				t.Errorf("deleted = %d, want %d", stats.Deleted, tt.wantDeleted)
			}
		})
	}
}

func TestEventRouting(t *testing.T) {
	tests := []struct {
		name    string
		event   Event
		wantErr error
		wantPri string
		wantSec string
	}{
		{
			name:    "main",
			event:   Event{Input: "SRC1", Tags: []string{"MAIN"}, StartDate: "2026/08/16 13:00:00", EndDate: "2026/08/16 14:00:00"},
			wantPri: "SRC1",
		},
		{
			name:    "backup",
			event:   Event{Input: "SRC2", Tags: []string{"BACKUP"}, StartDate: "2026/08/16 13:00:00", EndDate: "2026/08/16 14:00:00"},
			wantSec: "SRC2",
		},
		{
			name:    "no tag",
			event:   Event{Input: "SRC1", Tags: []string{"OTHER"}, StartDate: "2026/08/16 13:00:00", EndDate: "2026/08/16 14:00:00"},
			wantErr: ErrNoTag,
		},
		{
			// the routing is built whatever the current time, the store decides what's active
			name:    "window in the past",
			event:   Event{Input: "SRC1", Tags: []string{"MAIN"}, StartDate: "2020/01/01 13:00:00", EndDate: "2020/01/01 14:00:00"},
			wantPri: "SRC1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			br, err := tt.event.Routing()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}

			if err != nil {
				return
			}

			if br.Pri != tt.wantPri || br.Sec != tt.wantSec || br.StartDate.Format(DateLayout) != tt.event.StartDate ||
				br.EndDate.Format(DateLayout) != tt.event.EndDate {
				t.Errorf("routing = %+v, want pri=%q sec=%q", br, tt.wantPri, tt.wantSec)
			}
		})
	}

	t.Run("bad date", func(t *testing.T) {
		e := Event{Input: "SRC1", Tags: []string{"MAIN"}, StartDate: "2026-08-16T13:00:00Z", EndDate: "2026/08/16 14:00:00"}
		if _, err := e.Routing(); err == nil || errors.Is(err, ErrNoTag) {
			t.Fatalf("err = %v, want a parse error", err)
		}
	})
}

func TestIsActive(t *testing.T) {
	now := time.Date(2026, 8, 16, 13, 30, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }

	if !IsActive(now, at(0), at(0)) {
		t.Error("bounds should be inclusive")
	}

	if IsActive(now, at(time.Second), at(time.Hour)) || IsActive(now, at(-time.Hour), at(-time.Second)) {
		t.Error("outside the window should not be active")
	}

	if IsActive(now, nil, at(time.Hour)) || IsActive(now, at(-time.Hour), nil) {
		t.Error("nil bounds should never be active")
	}
}

/********** real lab data snapshotted by ../testdata/fetch_fixtures.py **********/

type schedulerFixture struct {
	ApgPair        []json.RawMessage `json:"apg_pair"`
	ApgTagCombos   []json.RawMessage `json:"apg_tag_combos"`
	NonApg         []json.RawMessage `json:"non_apg"`
	ScheduleShaped json.RawMessage   `json:"schedule_shaped"`
}

func loadSchedulerFixture(t *testing.T) schedulerFixture {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("..", "testdata", "scheduler_events.json"))
	if err != nil {
		t.Fatal(err)
	}

	var f schedulerFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}

	return f
}

func decodeOne(t *testing.T, payload json.RawMessage) Event {
	t.Helper()

	events, stats, err := ParseEvents(payload)
	if err != nil {
		t.Fatal(err)
	}

	if len(events) != 1 || stats.Invalid != 0 {
		t.Fatalf("parsed %d events with %d invalid, want 1 valid apg event", len(events), stats.Invalid)
	}

	return events[0]
}

// the events api sends start/end as numbers, the elasticsearch snapshots as strings
func TestParseEventsEpochTypes(t *testing.T) {
	const tmpl = `{"task":"apg","output":"BUS1","input":"SRC1","tags":["MAIN"],"start":%s,"end":%s,` +
		`"start_date":"2026/11/16 05:00:00","end_date":"2026/11/17 05:00:00"}`

	for name, payload := range map[string]string{
		"numbers": fmt.Sprintf(tmpl, "1794805200000000000", "1794891600000000000"),
		"strings": fmt.Sprintf(tmpl, `"1794805200000000000"`, `"1794891600000000000"`),
	} {
		t.Run(name, func(t *testing.T) {
			events, stats, err := ParseEvents([]byte(payload))
			if err != nil {
				t.Fatal(err)
			}

			if len(events) != 1 || stats.Invalid != 0 {
				t.Fatalf("events = %d stats = %+v, want 1 valid event", len(events), stats)
			}

			if events[0].Start.String() != "1794805200000000000" || events[0].End.String() != "1794891600000000000" {
				t.Errorf("start = %s end = %s", events[0].Start, events[0].End)
			}
		})
	}
}

func TestRealEventsDecode(t *testing.T) {
	f := loadSchedulerFixture(t)

	for _, payload := range slices.Concat(f.ApgPair, f.ApgTagCombos) {
		var raw struct {
			Tags  []string    `json:"tags"`
			Start json.Number `json:"start"`
		}
		if err := json.Unmarshal(payload, &raw); err != nil {
			t.Fatal(err)
		}

		e := decodeOne(t, payload)

		if e.Task != "apg" || e.Output == "" || e.Input == "" || e.Start != raw.Start || !slices.Equal(e.Tags, raw.Tags) {
			t.Errorf("decoded %s: %+v", payload[:80], e)
		}
	}
}

func TestRealEventsRouting(t *testing.T) {
	f := loadSchedulerFixture(t)

	for _, payload := range slices.Concat(f.ApgPair, f.ApgTagCombos) {
		e := decodeOne(t, payload)

		t.Run(e.Output+" "+e.Input, func(t *testing.T) {
			br, err := e.Routing()
			if err != nil {
				t.Fatalf("tags %v: %v", e.Tags, err)
			}

			// MAIN takes the primary even when combined with EXTERNAL or PxP
			switch {
			case slices.Contains(e.Tags, "MAIN"):
				if br.Pri != e.Input || br.Sec != "" {
					t.Errorf("tags %v: routing %+v, want pri=%s", e.Tags, br, e.Input)
				}
			case slices.Contains(e.Tags, "BACKUP"):
				if br.Sec != e.Input || br.Pri != "" {
					t.Errorf("tags %v: routing %+v, want sec=%s", e.Tags, br, e.Input)
				}
			}
		})
	}
}

func TestRealEventsMixedPayload(t *testing.T) {
	f := loadSchedulerFixture(t)

	payload, err := json.Marshal(slices.Concat(f.ApgPair, f.ApgTagCombos, f.NonApg, []json.RawMessage{f.ScheduleShaped}))
	if err != nil {
		t.Fatal(err)
	}

	events, stats, err := ParseEvents(payload)
	if err != nil {
		t.Fatal(err)
	}

	if want := len(f.ApgPair) + len(f.ApgTagCombos); len(events) != want {
		t.Errorf("apg events = %d, want %d", len(events), want)
	}

	// silence, xmuds, route and the schedule document (which nests apg events but has no task)
	if want := len(f.NonApg) + 1; stats.NonApg != want {
		t.Errorf("nonApg = %d, want %d", stats.NonApg, want)
	}

	if stats.Invalid != 0 {
		t.Errorf("invalid = %d, want 0", stats.Invalid)
	}
}

func BenchmarkParseEvents(b *testing.B) {
	items := make([]string, 0, 100)
	for i := range 100 {
		if i%10 == 0 {
			items = append(items, apgEventJSON("BUS1", "SRC1", "MAIN", "2026/08/16 13:00:00", "2026/08/16 14:00:00"))
			continue
		}
		items = append(items, `{"task":"router","output":"X","input":"Y","state":"running","history":{"a":[1,2,3]},"tags":["MAIN"]}`)
	}

	payload := []byte("[" + strings.Join(items, ",") + "]")
	if !json.Valid(payload) {
		b.Fatal("invalid benchmark payload")
	}

	b.ResetTimer()

	for range b.N {
		if _, _, err := ParseEvents(payload); err != nil {
			b.Fatal(err)
		}
	}
}
