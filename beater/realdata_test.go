package beater

// Tests using real lab data snapshotted into testdata/ by testdata/fetch_fixtures.py

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	insite "github.com/thetherington/routebeat/beater/analytics"
	routeCfg "github.com/thetherington/routebeat/config"
)

// source tags used for the zorro and tda sources. routebeat documents don't store source tags,
// so the replay supplies them from the recorded status
const (
	realZorroTag = "zorro-src"
	realTDATag   = "tda-src"
)

func loadFixture(t *testing.T, name string, v any) {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

func routingStateFromString(t *testing.T, s string) RoutingState {
	t.Helper()

	for state, name := range routingName {
		if name == s {
			return state
		}
	}

	t.Fatalf("unknown routing state %q", s)
	return Unknown
}

/********** scheduler events **********/

type schedulerFixture struct {
	ApgPair        []json.RawMessage `json:"apg_pair"`
	ApgTagCombos   []json.RawMessage `json:"apg_tag_combos"`
	NonApg         []json.RawMessage `json:"non_apg"`
	ScheduleShaped json.RawMessage   `json:"schedule_shaped"`
}

// activeAt returns a time inside the event's schedule window
func activeAt(t *testing.T, e SchedulerAPGEvent) time.Time {
	t.Helper()

	start, err := time.Parse(schedulerDateLayout, e.StartDate)
	if err != nil {
		t.Fatal(err)
	}

	return start.Add(time.Minute)
}

func decodeOne(t *testing.T, payload json.RawMessage) SchedulerAPGEvent {
	t.Helper()

	resetSchedulerEventCounters()

	events, err := parseSchedulerEvents(payload)
	if err != nil {
		t.Fatal(err)
	}

	if len(events) != 1 || atomic.LoadInt64(&schedulerEventCounters.invalid) != 0 {
		t.Fatalf("parsed %d events with %d invalid, want 1 valid apg event", len(events), schedulerEventCounters.invalid)
	}

	return events[0]
}

func TestRealSchedulerEventsDecode(t *testing.T) {
	var f schedulerFixture
	loadFixture(t, "scheduler_events.json", &f)

	for _, payload := range append(slices.Clone(f.ApgPair), f.ApgTagCombos...) {
		var raw struct {
			Tags  []string `json:"tags"`
			Start string   `json:"start"`
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

func TestRealSchedulerEventsBusRouting(t *testing.T) {
	var f schedulerFixture
	loadFixture(t, "scheduler_events.json", &f)

	for _, payload := range append(slices.Clone(f.ApgPair), f.ApgTagCombos...) {
		e := decodeOne(t, payload)

		t.Run(e.Output+" "+e.Input, func(t *testing.T) {
			br, err := e.busRouting(activeAt(t, e))
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

			// outside the window the event is discarded
			if _, err := e.busRouting(activeAt(t, e).Add(-2 * time.Minute)); !errors.Is(err, errEventInactive) {
				t.Errorf("before the window: err = %v, want errEventInactive", err)
			}
		})
	}
}

func TestRealSchedulerEventsMixedPayload(t *testing.T) {
	var f schedulerFixture
	loadFixture(t, "scheduler_events.json", &f)

	items := slices.Concat(f.ApgPair, f.ApgTagCombos, f.NonApg, []json.RawMessage{f.ScheduleShaped})

	payload, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}

	resetSchedulerEventCounters()

	events, err := parseSchedulerEvents(payload)
	if err != nil {
		t.Fatal(err)
	}

	if want := len(f.ApgPair) + len(f.ApgTagCombos); len(events) != want {
		t.Errorf("apg events = %d, want %d", len(events), want)
	}

	// silence, xmuds, route and the schedule document (which nests apg events but has no task)
	if got, want := atomic.LoadInt64(&schedulerEventCounters.nonApg), int64(len(f.NonApg)+1); got != want {
		t.Errorf("nonApg = %d, want %d", got, want)
	}

	if got := atomic.LoadInt64(&schedulerEventCounters.invalid); got != 0 {
		t.Errorf("invalid = %d, want 0", got)
	}
}

func TestRealSchedulerEventsPairUpdatesHotCache(t *testing.T) {
	var f schedulerFixture
	loadFixture(t, "scheduler_events.json", &f)

	main, backup := decodeOne(t, f.ApgPair[0]), decodeOne(t, f.ApgPair[1])
	now := activeAt(t, main)

	t.Run("single payload", func(t *testing.T) {
		resetCaches()

		payload, _ := json.Marshal(f.ApgPair)
		events, err := parseSchedulerEvents(payload)
		if err != nil {
			t.Fatal(err)
		}

		if got := applySchedulerEvents(events, now); got != 1 {
			t.Fatalf("updated = %d, want 1", got)
		}

		r, _ := scheduleHotCache.Get(main.Output)
		if r == nil || r.Pri != main.Input || r.Sec != backup.Input {
			t.Errorf("hot cache %s = %+v, want pri=%s sec=%s", main.Output, r, main.Input, backup.Input)
		}
	})

	t.Run("separate payloads merge", func(t *testing.T) {
		resetCaches()

		applySchedulerEvents([]SchedulerAPGEvent{main}, now)
		applySchedulerEvents([]SchedulerAPGEvent{backup}, now)

		r, _ := scheduleHotCache.Get(main.Output)
		if r == nil || r.Pri != main.Input || r.Sec != backup.Input {
			t.Errorf("hot cache %s = %+v, want pri=%s sec=%s", main.Output, r, main.Input, backup.Input)
		}
	})

	t.Run("events api response", func(t *testing.T) {
		resetCaches()

		// the real events are outside today's window, so check the request and payload path only
		var gotOutput string
		srv, calls := newEventsAPIStub(t, http.StatusOK, func(output string) string {
			gotOutput = output
			b, _ := json.Marshal(f.ApgPair)
			return string(b)
		})

		resetSchedulerEventCounters()

		if err := newTestEventsAPI(t, srv.URL).Refresh(context.Background(), main.Output); err != nil {
			t.Fatal(err)
		}

		if calls.Load() != 1 || gotOutput != main.Output {
			t.Errorf("calls = %d output = %q, want 1 call for %s", calls.Load(), gotOutput, main.Output)
		}

		// decoded and discarded as inactive rather than invalid
		if got := atomic.LoadInt64(&schedulerEventCounters.apgOutTime); got != 2 {
			t.Errorf("apgOutTime = %d, want 2", got)
		}
	})
}

/********** routebeat replay **********/

type rbTerminal struct {
	Name    string            `json:"name"`
	IsSub   bool              `json:"isSub"`
	IsSrc   bool              `json:"isSrc"`
	Nameset map[string]string `json:"nameset"`
}

type rbDoc struct {
	Timestamp         string            `json:"@timestamp"`
	EventType         string            `json:"eventType"`
	DstID             string            `json:"dstId"`
	DstName           string            `json:"dstName"`
	DstTag            string            `json:"dstTag"`
	DstTags           []string          `json:"dstTags"`
	DstType           string            `json:"dstType"`
	DstIsSub          bool              `json:"dstIsSub"`
	DstIsDst          bool              `json:"dstIsDst"`
	DstNameset        map[string]string `json:"dstNameset"`
	DestinationLabel  string            `json:"destinationLabel"`
	SourceLabel       string            `json:"sourceLabel"`
	RouteableTerminal struct {
		SubscribedSource *rbTerminal `json:"subscribedSource"`
		PhysicalSource   *rbTerminal `json:"physicalSource"`
	} `json:"routeableTerminal"`
	Schedule struct {
		Buscode string `json:"buscode"`
		Status  string `json:"status"`
		Primary string `json:"primary"`
		Backup  string `json:"backup"`
		Matched bool   `json:"matched"`
	} `json:"schedule"`
}

func namesets(m map[string]string) []NamesetName {
	out := make([]NamesetName, 0, len(m))
	for nameset, name := range m {
		out = append(out, NamesetName{Name: name, Nameset: Nameset{Name: nameset}})
	}
	return out
}

// edge rebuilds the graphql edge that produced the document. source tags aren't stored, so the
// zorro/tda tag is added from the recorded status
func (d rbDoc) edge() *Edge {
	e := &Edge{
		Id:           d.DstID,
		Name:         d.DstName,
		Tags:         d.DstTags,
		IsSub:        d.DstIsSub,
		IsDst:        d.DstIsDst,
		Type:         d.DstType,
		NamesetNames: namesets(d.DstNameset),
	}

	if s := d.RouteableTerminal.SubscribedSource; s != nil {
		e.SubscribedSource = &SubscribedSource{Name: s.Name, IsSub: s.IsSub, NamesetNames: namesets(s.Nameset)}

		switch d.Schedule.Status {
		case Zorro.String():
			e.SubscribedSource.Tags = []string{realZorroTag}
		case TDA.String():
			e.SubscribedSource.Tags = []string{realTDATag}
		}
	}

	if p := d.RouteableTerminal.PhysicalSource; p != nil {
		e.RoutedPhysicalSource = &RoutedPhysicalSource{Name: p.Name, IsSrc: p.IsSrc, NamesetNames: namesets(p.Nameset)}
	}

	return e
}

// seedRouting stores the document's schedule routing as an active ES routing
func (d rbDoc) seedRouting() {
	now := time.Now()
	scheduleCache.Set(d.Schedule.Buscode, []*insite.BusRouting{
		routing(d.Schedule.Primary, d.Schedule.Backup, now.Add(-time.Hour), now.Add(time.Hour)),
	})
}

func newRealBeat() *routebeat {
	// the routebeat documents store nameset keys lowercased
	return &routebeat{config: routeCfg.Config{
		Mapping: &routeCfg.Mapping{Nameset: "broadview"},
		Zorro:   realZorroTag,
		TDA:     realTDATag,
		ES:      routeCfg.Elasticsearch{Dev: routeCfg.Developer{ValidateEndDate: true}},
	}}
}

func TestRealRoutebeatReplay(t *testing.T) {
	var f struct {
		Cases []struct {
			Name   string `json:"name"`
			Expect string `json:"expect"`
			Note   string `json:"note"`
			Doc    rbDoc  `json:"doc"`
		} `json:"cases"`
	}
	loadFixture(t, "routebeat_events.json", &f)

	if len(f.Cases) == 0 {
		t.Fatal("no replay cases")
	}

	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			resetCaches()

			d := c.Doc
			if d.Schedule.Matched {
				d.seedRouting()
			}

			counters := &Counters{}

			res, err := newRealBeat().CreateEventFromEdge(d.edge(), d.DstTag, counters, Query)

			checkValue(t, "destinationLabel", fieldStr(t, res, "destinationLabel"), d.DestinationLabel)
			checkValue(t, "sourceLabel", fieldStr(t, res, "sourceLabel"), d.SourceLabel)

			if c.Expect == "not_found" {
				if !errors.Is(err, ErrScheduleBusNotFound) {
					t.Fatalf("err = %v, want ErrScheduleBusNotFound", err)
				}
				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if c.Note != "" {
				t.Log(c.Note)
			}

			assertSchedule(t, res, wantSchedule{
				matched: true,
				source:  "cache",
				status:  c.Expect,
				primary: d.Schedule.Primary,
				backup:  d.Schedule.Backup,
			})

			want := Counters{}
			want.Increment(routingStateFromString(t, c.Expect))
			assertCounters(t, counters, want)
		})
	}
}

func TestRealNotificationSequence(t *testing.T) {
	var f struct {
		Sequences []struct {
			Bus  string  `json:"bus"`
			Docs []rbDoc `json:"docs"`
		} `json:"sequences"`
	}
	loadFixture(t, "notification_sequence.json", &f)

	for _, seq := range f.Sequences {
		t.Run(seq.Bus, func(t *testing.T) {
			if len(seq.Docs) < 2 {
				t.Fatalf("sequence has %d notifications, want at least 2", len(seq.Docs))
			}

			resetCaches()
			useNotifier(t, stubNotifier{})

			// start from the first recorded notification and replay the rest in order
			first := seq.Docs[0]
			seedBus(first.Schedule.Buscode, routingStateFromString(t, first.Schedule.Status), first.SourceLabel, 0, 0, 0)

			bt := newRealBeat()

			for i, d := range seq.Docs[1:] {
				prev := seq.Docs[i]

				d.seedRouting()

				res, err := bt.CreateEventFromEdge(d.edge(), d.DstTag, &Counters{}, Notification)
				if err != nil {
					t.Fatalf("step %d (%s): %v", i+1, d.Timestamp, err)
				}

				status := fieldStr(t, res, "schedule.status")
				if status != d.Schedule.Status {
					t.Errorf("step %d (%s): status = %s, recorded %s", i+1, d.Timestamp, status, d.Schedule.Status)
					continue
				}

				if prev.Schedule.Status == d.Schedule.Status {
					assertNotifications(t, res.notifications, nil)
					continue
				}

				summary := "Error"
				if d.Schedule.Status == Primary.String() {
					summary = "Correction"
				}

				assertNotificationsFor(t, d.Schedule.Buscode, res.notifications, []wantNotif{
					{"previous", prev.Schedule.Status, prev.SourceLabel, summary, "", isNow},
					{"current", d.Schedule.Status, d.SourceLabel, summary, isNow, isEmpty},
				})

				if d.Schedule.Status == Primary.String() {
					checkValue(t, "deviationEndTime", fieldStr(t, res, "schedule.deviationEndTime"), isNow)
				}

				assertBus(t, d.Schedule.Buscode, routingStateFromString(t, d.Schedule.Status), d.SourceLabel)
			}
		})
	}
}
