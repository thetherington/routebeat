package beater

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thetherington/routebeat/beater/eventsapi"
	"github.com/thetherington/routebeat/beater/schedule"
	routeCfg "github.com/thetherington/routebeat/config"
)

func resetCaches() {
	scheduleStore.Clear()
	busCache.Load(make(map[string]*BusState))
}

func resetScheduleEventCounters() {
	atomic.StoreInt64(&scheduleEventCounters.events, 0)
	atomic.StoreInt64(&scheduleEventCounters.nonApg, 0)
	atomic.StoreInt64(&scheduleEventCounters.invalid, 0)
	atomic.StoreInt64(&scheduleEventCounters.deleted, 0)
	atomic.StoreInt64(&scheduleEventCounters.noTag, 0)
	atomic.StoreInt64(&scheduleEventCounters.routings, 0)
}

func apgEventJSON(output, input, tag, start, end string) string {
	return fmt.Sprintf(`{"task":"apg","output":%q,"input":%q,"tags":[%q],"start_date":%q,"end_date":%q,"state":1,"history":[]}`,
		output, input, tag, start, end)
}

func timePtr(t time.Time) *time.Time { return &t }

func routing(pri, sec string, start, end time.Time) *schedule.BusRouting {
	return &schedule.BusRouting{Pri: pri, Sec: sec, StartDate: timePtr(start), EndDate: timePtr(end)}
}

// setSchedule replaces the schedule store with one routing per output
func setSchedule(routes map[string]*schedule.BusRouting) {
	lists := make(map[string][]*schedule.BusRouting, len(routes))
	for output, r := range routes {
		lists[output] = []*schedule.BusRouting{r}
	}

	scheduleStore.Replace(lists, time.Now())
}

// window returns a start/end date pair (scheduler format, UTC) from now+from to now+to
func window(from, to time.Duration) (string, string) {
	now := time.Now().UTC()
	return now.Add(from).Format(schedule.DateLayout), now.Add(to).Format(schedule.DateLayout)
}

// activeWindow returns a start/end date pair (scheduler format, UTC) around now
func activeWindow() (string, string) {
	return window(-time.Hour, time.Hour)
}

// newTestEventsAPI creates an events api client with the apg task filter, as in New()
func newTestEventsAPI(t *testing.T, url string) *eventsapi.Client {
	t.Helper()

	c, err := eventsapi.New(eventsapi.Config{Url: url, Timeout: time.Second, BulkTimeout: time.Second, Task: "apg"})
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func TestRelativeTime(t *testing.T) {
	tests := map[time.Duration]string{
		0:                "now",
		25 * time.Minute: "now+25m",
		time.Hour:        "now+1h",
		90 * time.Minute: "now+90m",
		45 * time.Second: "now+45s",
	}

	for d, want := range tests {
		if got := relativeTime(d); got != want {
			t.Errorf("relativeTime(%s) = %q, want %q", d, got, want)
		}
	}
}

// pollStub serves the bulk poll: the body for each start value, or a 500 when the body is ""
type pollStub struct {
	mu      sync.Mutex
	bodies  map[string]string
	queries []url.Values
	during  func(start string) // runs while the request is being served
}

func (p *pollStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start := q.Get("start")

	p.mu.Lock()
	p.queries = append(p.queries, q)
	body := p.bodies[start]
	during := p.during
	p.mu.Unlock()

	if during != nil {
		during(start)
	}

	if body == "" {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Write([]byte(body))
}

func TestPollSchedule(t *testing.T) {
	nowStart, nowEnd := activeWindow()
	nextStart, nextEnd := window(time.Hour, 2*time.Hour)

	nowBody := "[" +
		apgEventJSON("BUS1", "PRI", "MAIN", nowStart, nowEnd) + "," +
		apgEventJSON("BUS1", "SEC", "BACKUP", nowStart, nowEnd) + "," +
		apgEventJSON("BUS2", "B_PRI", "MAIN", nowStart, nowEnd) + "," +
		apgEventJSON("BUS3", "X", "EXTERNAL", nowStart, nowEnd) + "]"

	aheadBody := "[" +
		apgEventJSON("BUS1", "NEXT_PRI", "MAIN", nextStart, nextEnd) + "," +
		// BUS2's current event is still running at the lookahead time
		apgEventJSON("BUS2", "B_PRI", "MAIN", nowStart, nowEnd) + "]"

	t.Run("builds the store from now and the lookahead", func(t *testing.T) {
		resetCaches()
		resetScheduleEventCounters()

		stub := &pollStub{bodies: map[string]string{"now": nowBody, "now+25m": aheadBody}}
		srv := httptest.NewServer(stub)
		defer srv.Close()

		// an output missing from the poll is removed
		setSchedule(map[string]*schedule.BusRouting{"GONE": routing("G", "", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))})

		pollSchedule(newTestEventsAPI(t, srv.URL), 25*time.Minute)

		if len(stub.queries) != 2 {
			t.Fatalf("requests = %d, want 2", len(stub.queries))
		}

		for i, at := range []string{"now", "now+25m"} {
			want := url.Values{"start": {at}, "end": {at}, "task": {"apg"}}
			if got := stub.queries[i]; got.Encode() != want.Encode() {
				t.Errorf("request %d query = %v, want %v", i, got, want)
			}
		}

		bus1 := scheduleStore.Routes("BUS1")
		if len(bus1) != 2 || bus1[0].Pri != "PRI" || bus1[0].Sec != "SEC" || bus1[1].Pri != "NEXT_PRI" {
			t.Errorf("BUS1 = %+v, want the current window then the upcoming one", bus1)
		}

		if bus2 := scheduleStore.Routes("BUS2"); len(bus2) != 1 {
			t.Errorf("BUS2 = %+v, want the duplicate window folded", bus2)
		}

		if scheduleStore.Length() != 2 {
			t.Errorf("outputs = %d, want 2 (BUS3 untagged, GONE removed)", scheduleStore.Length())
		}

		if got := atomic.LoadInt64(&scheduleEventCounters.events); got != 6 {
			t.Errorf("events counter = %d, want 6", got)
		}

		if got := atomic.LoadInt64(&scheduleEventCounters.noTag); got != 1 {
			t.Errorf("noTag counter = %d, want 1", got)
		}
	})

	t.Run("a failed now fetch keeps the previous schedule", func(t *testing.T) {
		resetCaches()

		stub := &pollStub{bodies: map[string]string{"now+25m": aheadBody}}
		srv := httptest.NewServer(stub)
		defer srv.Close()

		setSchedule(map[string]*schedule.BusRouting{"BUS1": routing("KEEP", "", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))})

		pollSchedule(newTestEventsAPI(t, srv.URL), 25*time.Minute)

		if len(stub.queries) != 1 {
			t.Errorf("requests = %d, want 1 (no lookahead request after a failure)", len(stub.queries))
		}

		if r, ok := scheduleStore.Active("BUS1", time.Now(), true); !ok || r.Pri != "KEEP" || scheduleStore.Length() != 1 {
			t.Errorf("BUS1 = %+v %t, want the previous schedule", r, ok)
		}
	})

	t.Run("a failed lookahead fetch still loads now", func(t *testing.T) {
		resetCaches()

		stub := &pollStub{bodies: map[string]string{"now": nowBody}}
		srv := httptest.NewServer(stub)
		defer srv.Close()

		pollSchedule(newTestEventsAPI(t, srv.URL), 25*time.Minute)

		if bus1 := scheduleStore.Routes("BUS1"); len(bus1) != 1 || bus1[0].Pri != "PRI" {
			t.Errorf("BUS1 = %+v, want the current window only", bus1)
		}
	})

	t.Run("a refresh during the poll is kept", func(t *testing.T) {
		resetCaches()

		stub := &pollStub{bodies: map[string]string{"now": nowBody, "now+25m": aheadBody}}
		stub.during = func(start string) {
			if start == "now+25m" {
				scheduleStore.ReplaceOutput("BUS1", []*schedule.BusRouting{
					routing("REFRESHED", "", time.Now().Add(-time.Minute), time.Now().Add(time.Hour)),
				}, time.Now())
			}
		}

		srv := httptest.NewServer(stub)
		defer srv.Close()

		pollSchedule(newTestEventsAPI(t, srv.URL), 25*time.Minute)

		if r, ok := scheduleStore.Active("BUS1", time.Now(), true); !ok || r.Pri != "REFRESHED" {
			t.Errorf("BUS1 = %+v %t, want the refreshed routing", r, ok)
		}

		if r, ok := scheduleStore.Active("BUS2", time.Now(), true); !ok || r.Pri != "B_PRI" {
			t.Errorf("BUS2 = %+v %t, want the polled routing", r, ok)
		}
	})
}

func TestRefreshOutput(t *testing.T) {
	nowStart, nowEnd := activeWindow()
	nextStart, nextEnd := window(time.Hour, 2*time.Hour)
	now := time.Now().UTC()

	nowBody := "[" +
		apgEventJSON("BUS1", "A1", "MAIN", nowStart, nowEnd) + "," +
		apgEventJSON("BUS2", "B1", "MAIN", nowStart, nowEnd) + "," +
		apgEventJSON("BUS1", "A2", "BACKUP", nowStart, nowEnd) + "," +
		apgEventJSON("BUS3", "C2", "BACKUP", nowStart, nowEnd) + "," +
		`{"task":"router","output":"X"}` + "]"

	aheadBody := "[" +
		apgEventJSON("BUS1", "A1", "MAIN", nowStart, nowEnd) + "," + // still active at the lookahead
		apgEventJSON("BUS1", "NEXT_PRI", "MAIN", nextStart, nextEnd) + "," +
		apgEventJSON("BUS1", "NEXT_SEC", "BACKUP", nextStart, nextEnd) + "]"

	refresh := func(t *testing.T, bodies map[string]string) (*pollStub, bool, error) {
		t.Helper()

		stub := &pollStub{bodies: bodies}
		srv := httptest.NewServer(stub)
		t.Cleanup(srv.Close)

		bt := &routebeat{
			config:    routeCfg.Config{Schedule: routeCfg.Schedule{Lookahead: 25 * time.Minute}},
			eventsAPI: newTestEventsAPI(t, srv.URL),
		}

		updated, err := bt.refreshOutput("BUS1")
		return stub, updated, err
	}

	seed := func() {
		resetCaches()
		setSchedule(map[string]*schedule.BusRouting{
			"BUS1": routing("OLD_PRI", "OLD_SEC", now.Add(-time.Hour), now.Add(time.Hour)),
			"BUS2": routing("", "OLD_SEC", now.Add(-time.Hour), now.Add(time.Hour)),
		})
	}

	t.Run("now and lookahead requests for the output, like the poll", func(t *testing.T) {
		seed()

		stub, updated, err := refresh(t, map[string]string{"now": nowBody, "now+25m": aheadBody})
		if err != nil || !updated {
			t.Fatalf("updated = %t err = %v", updated, err)
		}

		want := []url.Values{
			{"output": {"BUS1"}, "start": {"now"}, "end": {"now"}, "task": {"apg"}},
			{"output": {"BUS1"}, "start": {"now+25m"}, "end": {"now+25m"}, "task": {"apg"}},
		}

		if len(stub.queries) != len(want) {
			t.Fatalf("requests = %d, want %d", len(stub.queries), len(want))
		}

		for i, q := range stub.queries {
			if q.Encode() != want[i].Encode() {
				t.Errorf("request %d = %v, want %v", i+1, q, want[i])
			}
		}

		// the polled window is replaced by the current window (MAIN and BACKUP aren't adjacent in
		// the list) and the upcoming window. the window in both responses is stored once
		routes := scheduleStore.Routes("BUS1")
		if len(routes) != 2 || routes[0].Pri != "A1" || routes[0].Sec != "A2" ||
			routes[1].Pri != "NEXT_PRI" || routes[1].Sec != "NEXT_SEC" {
			t.Errorf("BUS1 = %+v, want the current and upcoming windows", routes)
		}

		// events for other outputs are ignored, the poll keeps those current
		if r, _, ok := GetBusRouting("BUS2", true); !ok || r.Pri != "" || r.Sec != "OLD_SEC" {
			t.Errorf("BUS2 = %+v %t, want the stored routing", r, ok)
		}

		if _, _, ok := GetBusRouting("BUS3", true); ok {
			t.Error("BUS3 was added from a refresh for BUS1")
		}
	})

	t.Run("a failed lookahead fetch still applies now", func(t *testing.T) {
		seed()

		_, updated, err := refresh(t, map[string]string{"now": nowBody})
		if err != nil || !updated {
			t.Fatalf("updated = %t err = %v", updated, err)
		}

		if routes := scheduleStore.Routes("BUS1"); len(routes) != 1 || routes[0].Pri != "A1" || routes[0].Sec != "A2" {
			t.Errorf("BUS1 = %+v, want the current window only", routes)
		}
	})

	t.Run("a failed now fetch keeps the polled windows", func(t *testing.T) {
		seed()

		stub, updated, err := refresh(t, map[string]string{"now+25m": aheadBody})
		if err == nil || updated {
			t.Fatalf("updated = %t err = %v, want an error", updated, err)
		}

		if len(stub.queries) != 1 {
			t.Errorf("requests = %d, want 1 (no lookahead request after a failure)", len(stub.queries))
		}

		if r, _, ok := GetBusRouting("BUS1", true); !ok || r.Pri != "OLD_PRI" {
			t.Errorf("BUS1 = %+v %t, want the polled routing", r, ok)
		}
	})

	t.Run("no events for the output keeps the polled windows", func(t *testing.T) {
		seed()

		other := "[" + apgEventJSON("BUS2", "B1", "MAIN", nowStart, nowEnd) + "]"

		_, updated, err := refresh(t, map[string]string{"now": other, "now+25m": other})
		if err != nil || updated {
			t.Fatalf("updated = %t err = %v, want not updated", updated, err)
		}

		if r, _, ok := GetBusRouting("BUS1", true); !ok || r.Pri != "OLD_PRI" || r.Sec != "OLD_SEC" {
			t.Errorf("BUS1 = %+v %t, want the polled routing", r, ok)
		}
	})
}

func TestGetBusRouting(t *testing.T) {
	now := time.Now()

	t.Run("active", func(t *testing.T) {
		resetCaches()
		setSchedule(map[string]*schedule.BusRouting{"BUS1": routing("PRI", "SEC", now.Add(-time.Hour), now.Add(time.Hour))})

		r, src, ok := GetBusRouting("BUS1", true)
		if !ok || src != SourceCache || r.Pri != "PRI" {
			t.Errorf("got %+v %s %t", r, src, ok)
		}
	})

	t.Run("not found", func(t *testing.T) {
		resetCaches()

		_, src, ok := GetBusRouting("BUS1", true)
		if ok || src != SourceNone {
			t.Errorf("got %s %t", src, ok)
		}
	})

	t.Run("expired", func(t *testing.T) {
		resetCaches()
		setSchedule(map[string]*schedule.BusRouting{"BUS1": routing("P", "S", now.Add(-2*time.Hour), now.Add(-time.Hour))})

		if _, src, ok := GetBusRouting("BUS1", true); ok || src != SourceNone {
			t.Error("expired routing was returned")
		}

		if _, _, ok := GetBusRouting("BUS1", false); !ok {
			t.Error("routing should be returned when end dates are not validated")
		}
	})
}

func TestSaveCaches(t *testing.T) {
	// the cache files are written to the working directory
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })

	resetCaches()
	now := time.Now().UTC()

	busCache.Set("BUS1", NewBusState(Backup, "SRC2"))
	setSchedule(map[string]*schedule.BusRouting{
		"ACTIVE":  routing("SRC1", "SRC2", now.Add(-time.Hour), now.Add(time.Hour)),
		"EXPIRED": routing("SRC3", "", now.Add(-2*time.Hour), now.Add(-time.Hour)),
	})

	saveCaches()

	// load the files back the same way New() does
	resetCaches()

	if err := busCache.LoadFromFile(BUSCACHE_FILE); err != nil {
		t.Fatal(err)
	}

	if bs, ok := busCache.Get("BUS1"); !ok || bs.State != Backup || bs.Source != "SRC2" {
		t.Errorf("bus cache = %+v %t", bs, ok)
	}

	if err := scheduleStore.LoadFromFile(SCHEDULE_FILE); err != nil {
		t.Fatal(err)
	}

	// expired windows are pruned before saving
	if r, _, ok := GetBusRouting("ACTIVE", true); !ok || r.Pri != "SRC1" || r.Sec != "SRC2" {
		t.Errorf("ACTIVE = %+v %t", r, ok)
	}

	if scheduleStore.Length() != 1 {
		t.Errorf("schedule store has %d outputs, want 1 (expired window saved)", scheduleStore.Length())
	}
}
