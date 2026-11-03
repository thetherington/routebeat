package beater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	insite "github.com/thetherington/routebeat/beater/analytics"
	"github.com/thetherington/routebeat/beater/eventsapi"
	"github.com/thetherington/routebeat/beater/notify"
	routeCfg "github.com/thetherington/routebeat/config"
)

type stubNotifier struct{}

func (stubNotifier) Send(...notify.Notification) error { return nil }
func (stubNotifier) GetOrigin() string                 { return "test" }

// activeWindow returns a start/end date pair (scheduler format, UTC) around now
func activeWindow() (string, string) {
	now := time.Now().UTC()
	return now.Add(-time.Hour).Format(schedulerDateLayout), now.Add(time.Hour).Format(schedulerDateLayout)
}

// newTestEventsAPI creates an events api client wired to the SchedulerHookCallback, as in New()
func newTestEventsAPI(t *testing.T, url string) *eventsapi.Client {
	t.Helper()

	c, err := eventsapi.New(eventsapi.Config{Url: url, Timeout: time.Second}, SchedulerHookCallback)
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func TestEventsAPIRefreshUpdatesHotCache(t *testing.T) {
	resetCaches()
	start, end := activeWindow()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		output := r.URL.Query().Get("output")
		w.Write([]byte("[" +
			apgEventJSON(output, "SRC1", "MAIN", start, end) + "," +
			apgEventJSON(output, "SRC2", "BACKUP", start, end) + "," +
			`{"task":"router","output":"X"}` + "]"))
	}))
	defer srv.Close()

	if err := newTestEventsAPI(t, srv.URL).Refresh(context.Background(), "BUS1"); err != nil {
		t.Fatal(err)
	}

	r, ok := scheduleHotCache.Get("BUS1")
	if !ok || r.Pri != "SRC1" || r.Sec != "SRC2" {
		t.Errorf("hot cache = %+v %t", r, ok)
	}
}

// a response for one output can carry events for other outputs, all of them are merged into the hot cache
func TestEventsAPIRefreshUpdatesMultipleOutputs(t *testing.T) {
	resetCaches()
	start, end := activeWindow()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("[" +
			apgEventJSON("BUS1", "A1", "MAIN", start, end) + "," +
			apgEventJSON("BUS2", "B1", "MAIN", start, end) + "," +
			apgEventJSON("BUS1", "A2", "BACKUP", start, end) + "," +
			apgEventJSON("BUS3", "C2", "BACKUP", start, end) + "]"))
	}))
	defer srv.Close()

	// an existing BUS2 entry with an overlapping window keeps its backup
	now := time.Now().UTC()
	scheduleHotCache.Set("BUS2", routing("", "OLD_SEC", now.Add(-time.Hour), now.Add(time.Hour)))

	if err := newTestEventsAPI(t, srv.URL).Refresh(context.Background(), "BUS1"); err != nil {
		t.Fatal(err)
	}

	want := map[string]struct{ pri, sec string }{
		"BUS1": {"A1", "A2"},      // requested output, MAIN and BACKUP are not adjacent in the list
		"BUS2": {"B1", "OLD_SEC"}, // other output merged with the existing entry
		"BUS3": {"", "C2"},        // other output with only a BACKUP event
	}

	for output, w := range want {
		r, ok := GetScheduleHotRoute(output, time.Now())
		if !ok || r.Pri != w.pri || r.Sec != w.sec {
			t.Errorf("%s = %+v %t, want pri=%q sec=%q", output, r, ok, w.pri, w.sec)
		}
	}
}

// the ScheduleHotCache (hit) and ScheduleHotCacheMiss selectors log the same fields for every hot cache check
func TestRecheckHotRouteLogs(t *testing.T) {
	const (
		hit  = "ScheduleHotCache"
		miss = "ScheduleHotCacheMiss"
	)

	if err := logp.DevelopmentSetup(logp.ToObserverOutput(), logp.WithSelectors(hit, miss)); err != nil {
		t.Fatal(err)
	}

	const wait = 40 * time.Millisecond
	start, end := activeWindow()
	original := &insite.BusRouting{Pri: "ES_PRI", Sec: "ES_SEC"}

	tests := []struct {
		name    string
		webhook bool
		body    string // events api response
		post    string // webhook payload delivered during the wait
		want    []string
	}{
		{
			name: "events api with no routing",
			body: "[]",
			want: []string{
				miss + ": Hot cache miss Destination:BUS1 Source:SRC_NEW Check:events_api Reason:no_active_routing Primary: Backup:",
			},
		},
		{
			name:    "events api and webhook wait with a different source",
			webhook: true,
			body:    "[" + apgEventJSON("BUS1", "OTHER_PRI", "MAIN", start, end) + "," + apgEventJSON("BUS1", "OTHER_SEC", "BACKUP", start, end) + "]",
			want: []string{
				miss + ": Hot cache miss Destination:BUS1 Source:SRC_NEW Check:events_api Reason:source_not_scheduled Primary:OTHER_PRI Backup:OTHER_SEC",
				miss + ": Hot cache miss Destination:BUS1 Source:SRC_NEW Check:webhook_wait Reason:source_not_scheduled Primary:OTHER_PRI Backup:OTHER_SEC",
			},
		},
		{
			name: "events api hit",
			body: "[" + apgEventJSON("BUS1", "SRC_NEW", "MAIN", start, end) + "," + apgEventJSON("BUS1", "OTHER_SEC", "BACKUP", start, end) + "]",
			want: []string{
				hit + ": Hot cache hit Destination:BUS1 Source:SRC_NEW Check:events_api State:Primary Primary:SRC_NEW Backup:OTHER_SEC",
			},
		},
		{
			name:    "events api miss then webhook wait hit",
			webhook: true,
			body:    "[]",
			post:    apgEventJSON("BUS1", "SRC_NEW", "BACKUP", start, end),
			want: []string{
				miss + ": Hot cache miss Destination:BUS1 Source:SRC_NEW Check:events_api Reason:no_active_routing Primary: Backup:",
				hit + ": Hot cache hit Destination:BUS1 Source:SRC_NEW Check:webhook_wait State:Backup Primary: Backup:SRC_NEW",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCaches()
			logp.ObserverLogs().TakeAll()

			srv, _ := newEventsAPIStub(t, http.StatusOK, func(string) string { return tt.body })

			bt := &routebeat{config: routeCfg.Config{Webhook: routeCfg.Webhook{Enabled: tt.webhook, Wait: wait}}}
			bt.eventsAPI = newTestEventsAPI(t, srv.URL)

			if tt.post != "" {
				go func() {
					time.Sleep(wait / 2)
					SchedulerHookCallback(context.Background(), []byte(tt.post))
				}()
			}

			bt.recheckHotRoute("BUS1", "SRC_NEW", original, SourceCache)

			var got []string
			for _, entry := range logp.ObserverLogs().TakeAll() {
				if entry.LoggerName == hit || entry.LoggerName == miss {
					got = append(got, entry.LoggerName+": "+entry.Message)
				}
			}

			if !slices.Equal(got, tt.want) {
				t.Errorf("hot cache logs:\n got  %q\n want %q", got, tt.want)
			}
		})
	}
}

func TestMatchScheduledState(t *testing.T) {
	tests := []struct {
		name     string
		srcLabel string
		routing  *insite.BusRouting
		want     RoutingState
	}{
		{name: "primary", srcLabel: "A", routing: &insite.BusRouting{Pri: "A", Sec: "B"}, want: Primary},
		{name: "backup", srcLabel: "B", routing: &insite.BusRouting{Pri: "A", Sec: "B"}, want: Backup},
		{name: "no match", srcLabel: "C", routing: &insite.BusRouting{Pri: "A", Sec: "B"}, want: Unsched},
		{name: "blank label with blank pri", srcLabel: "", routing: &insite.BusRouting{Sec: "B"}, want: Unsched},
		{name: "blank label with blank sec", srcLabel: "", routing: &insite.BusRouting{Pri: "A"}, want: Unsched},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchScheduledState(tt.srcLabel, tt.routing); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestResolveRoutingState(t *testing.T) {
	bt := &routebeat{config: routeCfg.Config{Zorro: "zorro", TDA: "tda"}}
	r := &insite.BusRouting{Pri: "A", Sec: "B"}

	if got := bt.resolveRoutingState("C", []string{"zorro"}, r); got != Zorro {
		t.Errorf("got %s, want Zorro", got)
	}

	if got := bt.resolveRoutingState("C", []string{"tda"}, r); got != TDA {
		t.Errorf("got %s, want TDA", got)
	}

	if got := bt.resolveRoutingState("A", []string{"tda"}, r); got != Primary {
		t.Errorf("got %s, want Primary", got)
	}

	if got := bt.resolveRoutingState("C", nil, r); got != Unsched {
		t.Errorf("got %s, want Unsched", got)
	}

	// a blank label is Unknown, even with a blank schedule label or a zorro/tda tag
	blank := &insite.BusRouting{Pri: "A"}
	for _, tags := range [][]string{nil, {"zorro"}, {"tda"}} {
		if got := bt.resolveRoutingState("", tags, blank); got != Unknown {
			t.Errorf("blank label with tags %v: got %s, want Unknown", tags, got)
		}
	}
}

func TestIsUnscheduledTransition(t *testing.T) {
	if !isUnscheduledTransition(Notification, Primary, Unsched) {
		t.Error("Primary -> Unsched notification should recheck")
	}

	if isUnscheduledTransition(Query, Primary, Unsched) {
		t.Error("query events should not recheck")
	}

	if isUnscheduledTransition(Notification, Unknown, Unsched) {
		t.Error("unknown previous state should not recheck")
	}

	if isUnscheduledTransition(Notification, Unsched, Unsched) {
		t.Error("no transition should not recheck")
	}
}

func TestRecheckHotRoute(t *testing.T) {
	const wait = 100 * time.Millisecond

	start, end := activeWindow()
	original := &insite.BusRouting{Pri: "ES_PRI", Sec: "ES_SEC"}

	// stub api that returns a MAIN routing matching the source, or a 500 when failAPI is set
	var calls, failAPI atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failAPI.Load() == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(apgEventJSON(r.URL.Query().Get("output"), "SRC_NEW", "MAIN", start, end)))
	}))
	defer srv.Close()

	tests := []struct {
		name      string
		webhook   bool
		api       bool
		failAPI   bool
		wantState RoutingState
		wantWait  bool
		wantCalls int32
	}{
		{name: "webhook only waits", webhook: true, wantState: Unsched, wantWait: true},
		{name: "api match returns without waiting", webhook: true, api: true, wantState: Primary, wantCalls: 1},
		{name: "api only does not wait", api: true, wantState: Primary, wantCalls: 1},
		{name: "api failure falls back to the webhook wait", webhook: true, api: true, failAPI: true, wantState: Unsched, wantWait: true, wantCalls: 1},
		{name: "api failure without webhook does not wait", api: true, failAPI: true, wantState: Unsched, wantCalls: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCaches()
			calls.Store(0)
			failAPI.Store(0)
			if tt.failAPI {
				failAPI.Store(1)
			}

			bt := &routebeat{config: routeCfg.Config{Webhook: routeCfg.Webhook{Enabled: tt.webhook, Wait: wait}}}
			if tt.api {
				bt.eventsAPI = newTestEventsAPI(t, srv.URL)
			}

			began := time.Now()
			state, r, src := bt.recheckHotRoute("BUS1", "SRC_NEW", original, SourceCache)
			elapsed := time.Since(began)

			if state != tt.wantState {
				t.Errorf("state = %s, want %s", state, tt.wantState)
			}

			if tt.wantState == Unsched && (r != original || src != SourceCache) {
				t.Errorf("unsched should keep the original routing and source, got %+v %s", r, src)
			}

			if tt.wantState != Unsched && (r.Pri != "SRC_NEW" || src != SourceHotCache) {
				t.Errorf("got %+v %s, want the hot routing", r, src)
			}

			if waited := elapsed >= wait; waited != tt.wantWait {
				t.Errorf("waited = %t (%s), want %t", waited, elapsed, tt.wantWait)
			}

			if got := calls.Load(); got != tt.wantCalls {
				t.Errorf("api calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}

	t.Run("webhook data arriving during the wait is used", func(t *testing.T) {
		resetCaches()

		bt := &routebeat{config: routeCfg.Config{Webhook: routeCfg.Webhook{Enabled: true, Wait: wait}}}

		go func() {
			time.Sleep(wait / 2)
			SchedulerHookCallback(context.Background(), []byte(apgEventJSON("BUS1", "SRC_NEW", "BACKUP", start, end)))
		}()

		state, r, src := bt.recheckHotRoute("BUS1", "SRC_NEW", original, SourceCache)
		if state != Backup || r.Sec != "SRC_NEW" || src != SourceHotCache {
			t.Errorf("got %s %+v %s", state, r, src)
		}
	})

	// stub api with latency: it only has the new event once ready is set
	var ready atomic.Bool
	var requested atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(time.Now().UnixNano())
		if !ready.Load() {
			w.Write([]byte("[]"))
			return
		}
		w.Write([]byte(apgEventJSON(r.URL.Query().Get("output"), "SRC_NEW", "MAIN", start, end)))
	}))
	defer slow.Close()

	t.Run("events api delay pauses before the request", func(t *testing.T) {
		resetCaches()
		ready.Store(false)

		bt := &routebeat{config: routeCfg.Config{EventsAPI: routeCfg.EventsAPI{Delay: wait}}}
		bt.eventsAPI = newTestEventsAPI(t, slow.URL)

		// the api receives the new data partway through the delay
		go func() {
			time.Sleep(wait / 2)
			ready.Store(true)
		}()

		began := time.Now()
		state, r, src := bt.recheckHotRoute("BUS1", "SRC_NEW", original, SourceCache)

		if state != Primary || r.Pri != "SRC_NEW" || src != SourceHotCache {
			t.Errorf("got %s %+v %s, want the hot routing", state, r, src)
		}

		if delayed := time.Duration(requested.Load() - began.UnixNano()); delayed < wait {
			t.Errorf("api requested after %s, want at least %s", delayed, wait)
		}
	})

	t.Run("events api delay match skips the webhook wait", func(t *testing.T) {
		resetCaches()
		ready.Store(true)

		bt := &routebeat{config: routeCfg.Config{
			Webhook:   routeCfg.Webhook{Enabled: true, Wait: 10 * wait},
			EventsAPI: routeCfg.EventsAPI{Delay: wait},
		}}
		bt.eventsAPI = newTestEventsAPI(t, slow.URL)

		began := time.Now()
		state, _, _ := bt.recheckHotRoute("BUS1", "SRC_NEW", original, SourceCache)
		elapsed := time.Since(began)

		if state != Primary {
			t.Errorf("state = %s, want Primary", state)
		}

		if elapsed < wait || elapsed >= 10*wait {
			t.Errorf("took %s, want the delay (%s) without the webhook wait", elapsed, wait)
		}
	})
}
