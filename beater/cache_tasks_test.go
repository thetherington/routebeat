package beater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	insite "github.com/thetherington/routebeat/beater/analytics"
	"github.com/thetherington/routebeat/beater/httpserver"
)

// fixed reference time for the active window tests
var testNow = time.Date(2026, 8, 16, 13, 30, 0, 0, time.UTC)

func resetCaches() {
	scheduleCache.Load(make(map[string][]*insite.BusRouting))
	scheduleHotCache.Load(make(map[string]*insite.BusRouting))
	busCache.Load(make(map[string]*BusState))
}

func resetSchedulerEventCounters() {
	atomic.StoreInt64(&schedulerEventCounters.nonApg, 0)
	atomic.StoreInt64(&schedulerEventCounters.invalid, 0)
	atomic.StoreInt64(&schedulerEventCounters.noTag, 0)
	atomic.StoreInt64(&schedulerEventCounters.apgInTime, 0)
	atomic.StoreInt64(&schedulerEventCounters.apgOutTime, 0)
}

func apgEventJSON(output, input, tag, start, end string) string {
	return fmt.Sprintf(`{"task":"apg","output":%q,"input":%q,"tags":[%q],"start_date":%q,"end_date":%q,"state":1,"history":[]}`,
		output, input, tag, start, end)
}

func timePtr(t time.Time) *time.Time { return &t }

func routing(pri, sec string, start, end time.Time) *insite.BusRouting {
	return &insite.BusRouting{Pri: pri, Sec: sec, StartDate: timePtr(start), EndDate: timePtr(end)}
}

func TestParseSchedulerEvents(t *testing.T) {
	apg := apgEventJSON("TLUPDT", "SRC1", "MAIN", "2026/08/16 13:00:00", "2026/08/16 14:00:00")

	tests := []struct {
		name        string
		payload     string
		wantErr     bool
		wantEvents  int
		wantNonApg  int64
		wantInvalid int64
	}{
		{name: "single object", payload: apg, wantEvents: 1},
		{name: "array", payload: "[" + apg + "," + apg + "]", wantEvents: 2},
		{name: "leading whitespace", payload: "\n\t " + apg, wantEvents: 1},
		{name: "non-apg object", payload: `{"task":"router","output":"X"}`, wantEvents: 0, wantNonApg: 1},
		{
			name:       "non-apg with a different shape",
			payload:    `[{"task":"router","state":"running","history":{"a":1}}]`,
			wantEvents: 0, wantNonApg: 1,
		},
		{
			name:       "apg value in another field",
			payload:    `[{"task":"router","comment":"apg"}]`,
			wantEvents: 0, wantNonApg: 1,
		},
		{
			name:       "task is not a string",
			payload:    `[{"task":5,"comment":"apg"}]`,
			wantEvents: 0, wantInvalid: 1,
		},
		{
			name:       "apg event that does not fit the struct",
			payload:    `[{"task":"apg","state":"running"}]`,
			wantEvents: 0, wantInvalid: 1,
		},
		{
			name:       "mixed array keeps valid items",
			payload:    "[" + apg + `,{"task":"router"},{"task":"apg","state":"bad"},` + apg + "]",
			wantEvents: 2, wantNonApg: 1, wantInvalid: 1,
		},
		// valid JSON that isn't an event is skipped without an error
		{name: "json string", payload: `"apg"`, wantEvents: 0, wantNonApg: 1},
		{name: "json number", payload: `42`, wantEvents: 0, wantNonApg: 1},
		{name: "json null", payload: `null`, wantEvents: 0, wantNonApg: 1},
		{name: "empty object", payload: `{}`, wantEvents: 0, wantNonApg: 1},
		{name: "empty array", payload: `[]`, wantEvents: 0},
		{name: "array of scalars", payload: `[1,"apg",null]`, wantEvents: 0, wantNonApg: 2, wantInvalid: 1},

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
			resetSchedulerEventCounters()

			events, err := parseSchedulerEvents([]byte(tt.payload))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}

			if len(events) != tt.wantEvents {
				t.Errorf("events = %d, want %d", len(events), tt.wantEvents)
			}

			if got := atomic.LoadInt64(&schedulerEventCounters.nonApg); got != tt.wantNonApg {
				t.Errorf("nonApg = %d, want %d", got, tt.wantNonApg)
			}

			if got := atomic.LoadInt64(&schedulerEventCounters.invalid); got != tt.wantInvalid {
				t.Errorf("invalid = %d, want %d", got, tt.wantInvalid)
			}
		})
	}
}

// the webhook rejects only payloads that aren't JSON or are malformed JSON. everything else is accepted,
// and only active apg events reach the hot cache
func TestSchedulerWebhookResponses(t *testing.T) {
	start, end := activeWindow()
	past := time.Now().UTC()
	pastStart, pastEnd := past.Add(-2*time.Hour).Format(schedulerDateLayout), past.Add(-time.Hour).Format(schedulerDateLayout)

	tests := []struct {
		name       string
		payload    string
		wantStatus int
		wantCached bool
	}{
		{name: "active apg event", payload: apgEventJSON("BUS1", "SRC1", "MAIN", start, end), wantStatus: http.StatusAccepted, wantCached: true},
		{name: "active apg event in an array", payload: "[" + apgEventJSON("BUS1", "SRC1", "MAIN", start, end) + "]", wantStatus: http.StatusAccepted, wantCached: true},
		{name: "inactive apg event", payload: apgEventJSON("BUS1", "SRC1", "MAIN", pastStart, pastEnd), wantStatus: http.StatusAccepted},
		{name: "apg event without a MAIN or BACKUP tag", payload: apgEventJSON("BUS1", "SRC1", "EXTERNAL", start, end), wantStatus: http.StatusAccepted},
		{name: "apg event with a bad date", payload: apgEventJSON("BUS1", "SRC1", "MAIN", "tomorrow", end), wantStatus: http.StatusAccepted},
		{name: "apg event that doesn't fit the struct", payload: `{"task":"apg","state":"running"}`, wantStatus: http.StatusAccepted},
		{name: "non-apg event", payload: `{"task":"silence","event_params":{"silence":true}}`, wantStatus: http.StatusAccepted},
		{name: "array of non-apg events", payload: `[{"task":"route"},{"task":"xmuds","event_params":{"data":["a"]}}]`, wantStatus: http.StatusAccepted},
		{name: "json scalar", payload: `"hello"`, wantStatus: http.StatusAccepted},
		{name: "empty array", payload: `[]`, wantStatus: http.StatusAccepted},
		{name: "not json", payload: `task=apg&output=BUS1`, wantStatus: http.StatusBadRequest},
		{name: "malformed object", payload: `{"task":"apg","output":`, wantStatus: http.StatusBadRequest},
		{name: "malformed array", payload: `[{"task":"apg"},`, wantStatus: http.StatusBadRequest},
		{name: "whitespace only", payload: " \n\t ", wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCaches()

			s := httpserver.New(8082, nil, SchedulerHookCallback)

			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/scheduler/events", strings.NewReader(tt.payload)))

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", w.Code, tt.wantStatus, w.Body.String())
			}

			if cached := scheduleHotCache.Has("BUS1"); cached != tt.wantCached {
				t.Errorf("hot cache has BUS1 = %t, want %t", cached, tt.wantCached)
			}
		})
	}
}

func TestSchedulerAPGEventBusRouting(t *testing.T) {
	tests := []struct {
		name    string
		event   SchedulerAPGEvent
		wantErr error
		wantPri string
		wantSec string
	}{
		{
			name:    "main",
			event:   SchedulerAPGEvent{Input: "SRC1", Tags: []string{"MAIN"}, StartDate: "2026/08/16 13:00:00", EndDate: "2026/08/16 14:00:00"},
			wantPri: "SRC1",
		},
		{
			name:    "backup",
			event:   SchedulerAPGEvent{Input: "SRC2", Tags: []string{"BACKUP"}, StartDate: "2026/08/16 13:00:00", EndDate: "2026/08/16 14:00:00"},
			wantSec: "SRC2",
		},
		{
			name:    "no tag",
			event:   SchedulerAPGEvent{Input: "SRC1", Tags: []string{"OTHER"}, StartDate: "2026/08/16 13:00:00", EndDate: "2026/08/16 14:00:00"},
			wantErr: errEventNoTag,
		},
		{
			name:    "before window",
			event:   SchedulerAPGEvent{Input: "SRC1", Tags: []string{"MAIN"}, StartDate: "2026/08/16 13:31:00", EndDate: "2026/08/16 14:00:00"},
			wantErr: errEventInactive,
		},
		{
			name:    "after window",
			event:   SchedulerAPGEvent{Input: "SRC1", Tags: []string{"MAIN"}, StartDate: "2026/08/16 12:00:00", EndDate: "2026/08/16 13:29:59"},
			wantErr: errEventInactive,
		},
		{
			name:    "window bounds are inclusive",
			event:   SchedulerAPGEvent{Input: "SRC1", Tags: []string{"MAIN"}, StartDate: "2026/08/16 13:30:00", EndDate: "2026/08/16 13:30:00"},
			wantPri: "SRC1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			br, err := tt.event.busRouting(testNow)
			if err != tt.wantErr {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}

			if err != nil {
				return
			}

			if br.Pri != tt.wantPri || br.Sec != tt.wantSec {
				t.Errorf("routing = %+v, want pri=%q sec=%q", br, tt.wantPri, tt.wantSec)
			}
		})
	}

	t.Run("bad date", func(t *testing.T) {
		e := SchedulerAPGEvent{Input: "SRC1", Tags: []string{"MAIN"}, StartDate: "2026-08-16T13:00:00Z", EndDate: "2026/08/16 14:00:00"}
		if _, err := e.busRouting(testNow); err == nil {
			t.Fatal("expected a parse error")
		}
	})
}

func TestMergeBusRouting(t *testing.T) {
	start, end := testNow.Add(-30*time.Minute), testNow.Add(30*time.Minute)

	t.Run("nil existing returns a copy", func(t *testing.T) {
		incoming := routing("SRC1", "", start, end)

		got := mergeBusRouting(nil, incoming)
		if got == incoming || *got != *incoming {
			t.Errorf("got %+v, want a copy of %+v", got, incoming)
		}
	})

	t.Run("overlapping window merges", func(t *testing.T) {
		existing := routing("OLD", "SEC", start, end)
		incoming := routing("NEW", "", start.Add(time.Minute), end)

		got := mergeBusRouting(existing, incoming)
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

		got := mergeBusRouting(existing, incoming)
		if got.Pri != "NEW" || got.Sec != "" {
			t.Errorf("got %+v, want the previous window's Sec dropped", got)
		}
	})
}

func TestApplySchedulerEvents(t *testing.T) {
	resetCaches()
	resetSchedulerEventCounters()

	events := []SchedulerAPGEvent{
		{Task: "apg", Output: "BUS1", Input: "SRC1", Tags: []string{"MAIN"}, StartDate: "2026/08/16 13:00:00", EndDate: "2026/08/16 14:00:00"},
		{Task: "apg", Output: "BUS1", Input: "SRC2", Tags: []string{"BACKUP"}, StartDate: "2026/08/16 13:00:00", EndDate: "2026/08/16 14:00:00"},
		{Task: "apg", Output: "BUS2", Input: "SRC3", Tags: []string{}, StartDate: "2026/08/16 13:00:00", EndDate: "2026/08/16 14:00:00"},
		{Task: "apg", Output: "BUS3", Input: "SRC4", Tags: []string{"MAIN"}, StartDate: "2026/08/16 15:00:00", EndDate: "2026/08/16 16:00:00"},
	}

	if got := applySchedulerEvents(events, testNow); got != 1 {
		t.Fatalf("updated = %d, want 1", got)
	}

	held, _ := scheduleHotCache.Get("BUS1")
	if held.Pri != "SRC1" || held.Sec != "SRC2" {
		t.Fatalf("BUS1 = %+v, want pri=SRC1 sec=SRC2", held)
	}

	if scheduleHotCache.Has("BUS2") || scheduleHotCache.Has("BUS3") {
		t.Error("untagged or inactive events were cached")
	}

	if got := atomic.LoadInt64(&schedulerEventCounters.noTag); got != 1 {
		t.Errorf("noTag = %d, want 1", got)
	}

	if got := atomic.LoadInt64(&schedulerEventCounters.apgOutTime); got != 1 {
		t.Errorf("apgOutTime = %d, want 1", got)
	}

	// a later MAIN-only update keeps the Sec and does not mutate the routing a reader already holds
	applySchedulerEvents([]SchedulerAPGEvent{
		{Task: "apg", Output: "BUS1", Input: "SRC9", Tags: []string{"MAIN"}, StartDate: "2026/08/16 13:00:00", EndDate: "2026/08/16 14:00:00"},
	}, testNow)

	updated, _ := scheduleHotCache.Get("BUS1")
	if updated.Pri != "SRC9" || updated.Sec != "SRC2" {
		t.Errorf("BUS1 = %+v, want pri=SRC9 sec=SRC2", updated)
	}

	if held.Pri != "SRC1" {
		t.Error("routing held by a reader was mutated in place")
	}
}

func TestSchedulerHookCallbackConcurrentReads(t *testing.T) {
	resetCaches()

	// time.Parse returns UTC, so build the window in UTC for the active check
	now := time.Now().UTC()
	start, end := now.Add(-time.Hour).Format(schedulerDateLayout), now.Add(time.Hour).Format(schedulerDateLayout)

	var wg sync.WaitGroup

	for i := range 50 {
		wg.Add(2)

		go func() {
			defer wg.Done()
			payload := apgEventJSON("BUS1", fmt.Sprintf("SRC%d", i), "MAIN", start, end)
			if err := SchedulerHookCallback(context.Background(), []byte(payload)); err != nil {
				t.Error(err)
			}
		}()

		go func() {
			defer wg.Done()
			if r, _, ok := GetBusRoutingFromCaches("BUS1", true, true); ok {
				_ = r.Pri + r.Sec
			}
		}()
	}

	wg.Wait()
}

func TestGetBusRoutingFromCaches(t *testing.T) {
	now := time.Now()
	active := routing("ES_PRI", "ES_SEC", now.Add(-time.Hour), now.Add(time.Hour))
	hot := routing("HOT_PRI", "", now.Add(-time.Minute), now.Add(time.Hour))

	t.Run("cache only", func(t *testing.T) {
		resetCaches()
		scheduleCache.Set("BUS1", []*insite.BusRouting{active})

		r, src, ok := GetBusRoutingFromCaches("BUS1", true, true)
		if !ok || src != SourceCache || r.Pri != "ES_PRI" {
			t.Errorf("got %+v %s %t", r, src, ok)
		}
	})

	t.Run("hot cache wins", func(t *testing.T) {
		resetCaches()
		scheduleCache.Set("BUS1", []*insite.BusRouting{active})
		scheduleHotCache.Set("BUS1", hot)

		r, src, ok := GetBusRoutingFromCaches("BUS1", true, true)
		if !ok || src != SourceHotCache || r.Pri != "HOT_PRI" {
			t.Errorf("got %+v %s %t", r, src, ok)
		}
	})

	t.Run("hot cache disabled", func(t *testing.T) {
		resetCaches()
		scheduleCache.Set("BUS1", []*insite.BusRouting{active})
		scheduleHotCache.Set("BUS1", hot)

		r, src, ok := GetBusRoutingFromCaches("BUS1", true, false)
		if !ok || src != SourceCache || r.Pri != "ES_PRI" {
			t.Errorf("got %+v %s %t", r, src, ok)
		}
	})

	t.Run("hot cache only", func(t *testing.T) {
		resetCaches()
		scheduleHotCache.Set("BUS1", hot)

		_, src, ok := GetBusRoutingFromCaches("BUS1", true, true)
		if !ok || src != SourceHotCache {
			t.Errorf("got %s %t", src, ok)
		}
	})

	t.Run("not found", func(t *testing.T) {
		resetCaches()

		_, src, ok := GetBusRoutingFromCaches("BUS1", true, true)
		if ok || src != SourceNone {
			t.Errorf("got %s %t", src, ok)
		}
	})

	t.Run("expired cache entry", func(t *testing.T) {
		resetCaches()
		scheduleCache.Set("BUS1", []*insite.BusRouting{routing("P", "S", now.Add(-2*time.Hour), now.Add(-time.Hour))})

		if _, _, ok := GetBusRoutingFromCaches("BUS1", true, true); ok {
			t.Error("expired routing was returned")
		}

		if _, _, ok := GetBusRoutingFromCaches("BUS1", false, true); !ok {
			t.Error("routing should be returned when end dates are not validated")
		}
	})
}

func TestPruneScheduleHotCache(t *testing.T) {
	now := time.Now()

	seed := func() {
		resetCaches()
		scheduleHotCache.Set("ACTIVE", routing("A", "", now.Add(-time.Hour), now.Add(time.Hour)))
		scheduleHotCache.Set("FUTURE", routing("F", "", now.Add(time.Hour), now.Add(2*time.Hour)))
		scheduleHotCache.Set("EXPIRED", routing("E", "", now.Add(-2*time.Hour), now.Add(-time.Second)))
		scheduleHotCache.Set("NODATES", &insite.BusRouting{Pri: "N"})
	}

	check := func(t *testing.T, removed, want int) {
		t.Helper()

		if removed != want {
			t.Errorf("removed = %d, want %d", removed, want)
		}

		for _, key := range []string{"ACTIVE", "FUTURE"} {
			if !scheduleHotCache.Has(key) {
				t.Errorf("%s should be kept", key)
			}
		}

		for _, key := range []string{"EXPIRED", "NODATES", "NIL"} {
			if scheduleHotCache.Has(key) {
				t.Errorf("%s should be removed", key)
			}
		}
	}

	t.Run("in memory", func(t *testing.T) {
		seed()
		scheduleHotCache.Set("NIL", nil)
		check(t, PruneScheduleHotCache(now), 3)
	})

	t.Run("after load from file", func(t *testing.T) {
		seed()

		file := t.TempDir() + "/" + HOTCACHE_FILE
		if err := scheduleHotCache.SaveToFile(file); err != nil {
			t.Fatal(err)
		}

		resetCaches()
		if err := scheduleHotCache.LoadFromFile(file); err != nil {
			t.Fatal(err)
		}

		check(t, PruneScheduleHotCache(now), 2)
	})
}

func BenchmarkParseSchedulerEvents(b *testing.B) {
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
		if _, err := parseSchedulerEvents(payload); err != nil {
			b.Fatal(err)
		}
	}
}
