package beater

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thetherington/routebeat/beater/notify"
	"github.com/thetherington/routebeat/beater/schedule"
	routeCfg "github.com/thetherington/routebeat/config"
)

type stubNotifier struct{}

func (stubNotifier) Send(...notify.Notification) error { return nil }
func (stubNotifier) GetOrigin() string                 { return "test" }

// recheckBeat returns a beat with the events api at url ("" = none) and end date validation on
func recheckBeat(t *testing.T, url string, delay time.Duration) *routebeat {
	t.Helper()

	bt := &routebeat{config: routeCfg.Config{
		Schedule:  routeCfg.Schedule{Dev: routeCfg.Developer{ValidateEndDate: true}},
		EventsAPI: routeCfg.EventsAPI{Delay: delay},
	}}

	if url != "" {
		bt.eventsAPI = newTestEventsAPI(t, url)
	}

	return bt
}

// the ScheduleRecheck (hit) and ScheduleRecheckMiss selectors log the same fields for every recheck
func TestRecheckRouteLogs(t *testing.T) {
	const (
		hit  = "ScheduleRecheck"
		miss = "ScheduleRecheckMiss"
	)

	logs := observeLogs(t, hit, miss)

	start, end := activeWindow()
	original := &schedule.BusRouting{Pri: "POLL_PRI", Sec: "POLL_SEC"}

	tests := []struct {
		name   string
		polled bool // the store has the polled POLL_PRI/POLL_SEC routing
		status int
		body   string // events api response
		want   string
	}{
		{
			name:   "no routing",
			status: http.StatusOK,
			body:   "[]",
			want:   miss + ": Recheck miss Destination:BUS1 Source:SRC_NEW Reason:no_active_routing Primary: Backup:",
		},
		{
			name:   "refreshed routing with a different source",
			polled: true,
			status: http.StatusOK,
			body:   "[" + apgEventJSON("BUS1", "OTHER_PRI", "MAIN", start, end) + "," + apgEventJSON("BUS1", "OTHER_SEC", "BACKUP", start, end) + "]",
			want:   miss + ": Recheck miss Destination:BUS1 Source:SRC_NEW Reason:source_not_scheduled Primary:OTHER_PRI Backup:OTHER_SEC",
		},
		{
			name:   "refresh error checks the polled routing",
			polled: true,
			status: http.StatusInternalServerError,
			want:   miss + ": Recheck miss Destination:BUS1 Source:SRC_NEW Reason:source_not_scheduled Primary:POLL_PRI Backup:POLL_SEC",
		},
		{
			name:   "hit",
			status: http.StatusOK,
			body:   "[" + apgEventJSON("BUS1", "SRC_NEW", "MAIN", start, end) + "," + apgEventJSON("BUS1", "OTHER_SEC", "BACKUP", start, end) + "]",
			want:   hit + ": Recheck hit Destination:BUS1 Source:SRC_NEW State:Primary Primary:SRC_NEW Backup:OTHER_SEC",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCaches()
			logs.TakeAll()

			if tt.polled {
				now := time.Now().UTC()
				setSchedule(map[string]*schedule.BusRouting{"BUS1": routing("POLL_PRI", "POLL_SEC", now.Add(-time.Hour), now.Add(time.Hour))})
			}

			srv, _ := newEventsAPIStub(t, tt.status, func(string) string { return tt.body })

			recheckBeat(t, srv.URL, 0).recheckRoute("BUS1", "SRC_NEW", original, SourceCache)

			var got []string
			for _, entry := range logs.TakeAll() {
				if entry.LoggerName == hit || entry.LoggerName == miss {
					got = append(got, entry.LoggerName+": "+entry.Message)
				}
			}

			if !slices.Equal(got, []string{tt.want}) {
				t.Errorf("recheck logs:\n got  %q\n want %q", got, []string{tt.want})
			}
		})
	}
}

func TestMatchScheduledState(t *testing.T) {
	tests := []struct {
		name     string
		srcLabel string
		routing  *schedule.BusRouting
		want     RoutingState
	}{
		{name: "primary", srcLabel: "A", routing: &schedule.BusRouting{Pri: "A", Sec: "B"}, want: Primary},
		{name: "backup", srcLabel: "B", routing: &schedule.BusRouting{Pri: "A", Sec: "B"}, want: Backup},
		{name: "no match", srcLabel: "C", routing: &schedule.BusRouting{Pri: "A", Sec: "B"}, want: Unsched},
		{name: "blank label with blank pri", srcLabel: "", routing: &schedule.BusRouting{Sec: "B"}, want: Unsched},
		{name: "blank label with blank sec", srcLabel: "", routing: &schedule.BusRouting{Pri: "A"}, want: Unsched},
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
	r := &schedule.BusRouting{Pri: "A", Sec: "B"}

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
	blank := &schedule.BusRouting{Pri: "A"}
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

func TestRecheckRoute(t *testing.T) {
	start, end := activeWindow()
	original := &schedule.BusRouting{Pri: "POLL_PRI", Sec: "POLL_SEC"}

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
		api       bool
		failAPI   bool
		wantState RoutingState
		wantCalls int32
	}{
		{name: "no events api", wantState: Unsched},
		{name: "api match", api: true, wantState: Primary, wantCalls: 2},                  // now and lookahead
		{name: "api failure", api: true, failAPI: true, wantState: Unsched, wantCalls: 1}, // no lookahead after a failed now
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCaches()
			calls.Store(0)
			failAPI.Store(0)
			if tt.failAPI {
				failAPI.Store(1)
			}

			url := ""
			if tt.api {
				url = srv.URL
			}

			state, r, src := recheckBeat(t, url, 0).recheckRoute("BUS1", "SRC_NEW", original, SourceCache)

			if state != tt.wantState {
				t.Errorf("state = %s, want %s", state, tt.wantState)
			}

			if tt.wantState == Unsched && (r != original || src != SourceCache) {
				t.Errorf("unsched should keep the original routing and source, got %+v %s", r, src)
			}

			if tt.wantState != Unsched && (r.Pri != "SRC_NEW" || src != SourceRecheck) {
				t.Errorf("got %+v %s, want the refreshed routing", r, src)
			}

			if got := calls.Load(); got != tt.wantCalls {
				t.Errorf("api calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}

	t.Run("events api delay pauses before the request", func(t *testing.T) {
		const delay = 100 * time.Millisecond

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

		resetCaches()

		// the api receives the new data partway through the delay
		go func() {
			time.Sleep(delay / 2)
			ready.Store(true)
		}()

		began := time.Now()
		state, r, src := recheckBeat(t, slow.URL, delay).recheckRoute("BUS1", "SRC_NEW", original, SourceCache)

		if state != Primary || r.Pri != "SRC_NEW" || src != SourceRecheck {
			t.Errorf("got %s %+v %s, want the refreshed routing", state, r, src)
		}

		if delayed := time.Duration(requested.Load() - began.UnixNano()); delayed < delay {
			t.Errorf("api requested after %s, want at least %s", delayed, delay)
		}
	})
}
