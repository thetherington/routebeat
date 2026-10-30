package beater

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	insite "github.com/thetherington/routebeat/beater/analytics"
	"github.com/thetherington/routebeat/beater/notify"
	routeCfg "github.com/thetherington/routebeat/config"
)

// wait used for the webhook in tests
const testWait = 50 * time.Millisecond

// sentinel values for checkValue
const (
	isNow   = "<now>"   // RFC3339 time within 2 seconds of now
	isEmpty = "<empty>" // empty string
)

type beatOpts struct {
	noMapping  bool   // disable the Broadview nameset mapping
	defLabel   string // mapping default label
	webhook    bool
	eventsAPI  string // stub server url, "" = disabled
	noValidate bool   // disable ES.Dev.ValidateEndDate
}

func newTestBeat(t *testing.T, o beatOpts) *routebeat {
	t.Helper()

	cfg := routeCfg.Config{
		Zorro:   "zorro",
		TDA:     "tda",
		ES:      routeCfg.Elasticsearch{Dev: routeCfg.Developer{ValidateEndDate: !o.noValidate}},
		Webhook: routeCfg.Webhook{Enabled: o.webhook, Wait: testWait},
	}

	if !o.noMapping {
		cfg.Mapping = &routeCfg.Mapping{Nameset: "Broadview", Default: o.defLabel}
	}

	bt := &routebeat{config: cfg}
	if o.eventsAPI != "" {
		bt.eventsAPI = newTestEventsAPI(t, o.eventsAPI)
	}

	return bt
}

type edgeOpts struct {
	tags         []string // default {"MES"}
	src          string   // subscribed source label, "" = no subscribed source
	srcTags      []string
	physical     string // routed physical source label, "" = none
	noDstNameset bool   // leave out the destination Broadview nameset
	noSrcNameset bool   // leave out the subscribed source Broadview nameset
}

func newTestEdge(dst string, o edgeOpts) *Edge {
	broadview := Nameset{Name: "Broadview"}

	if o.tags == nil {
		o.tags = []string{"MES"}
	}

	e := &Edge{
		Id:    "id-" + dst,
		Name:  dst,
		Tags:  o.tags,
		IsDst: true,
		IsSub: true,
		Type:  "dst",
	}

	if !o.noDstNameset {
		e.NamesetNames = []NamesetName{{Name: dst, Nameset: broadview}}
	}

	if o.src != "" {
		e.SubscribedSource = &SubscribedSource{
			Name:  o.src,
			IsSub: true,
			Tags:  o.srcTags,
		}

		if !o.noSrcNameset {
			e.SubscribedSource.NamesetNames = []NamesetName{{Name: o.src, Nameset: broadview}}
		}
	}

	if o.physical != "" {
		e.RoutedPhysicalSource = &RoutedPhysicalSource{
			Name:         o.physical,
			IsSrc:        true,
			NamesetNames: []NamesetName{{Name: o.physical, Nameset: broadview}},
		}
	}

	return e
}

func ago(d time.Duration) *time.Time {
	if d == 0 {
		return nil
	}
	return timePtr(time.Now().Add(-d))
}

func fmtTime(t *time.Time) string {
	return t.Format(time.RFC3339)
}

// seedBus stores a BusState in the busCache with transition times relative to now (0 = nil)
func seedBus(dst string, state RoutingState, src string, transitionAgo, startAgo, restoreAgo time.Duration) *BusState {
	bs := &BusState{
		State:           state,
		Source:          src,
		Transition:      ago(transitionAgo),
		TransitionStart: ago(startAgo),
		Restore:         ago(restoreAgo),
	}

	busCache.Set(dst, bs)

	return bs
}

// useNotifier installs the notifier for the test and restores the previous one
func useNotifier(t *testing.T, n notify.NotiferInterface) {
	t.Helper()

	prev := notifier
	notifier = n
	t.Cleanup(func() { notifier = prev })
}

// newEventsAPIStub serves body(output) with a 200, or the status code with no body, and counts calls
func newEventsAPIStub(t *testing.T, status int, body func(output string) string) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}

		w.Write([]byte(body(r.URL.Query().Get("output"))))
	}))
	t.Cleanup(srv.Close)

	return srv, &calls
}

// postWebhookAfter delivers a webhook payload to the SchedulerHookCallback after a delay
func postWebhookAfter(d time.Duration, payload string) {
	go func() {
		time.Sleep(d)
		SchedulerHookCallback(context.Background(), []byte(payload))
	}()
}

func field(res *EventResults, key string) (any, bool) {
	v, err := res.event.GetValue(key)
	return v, err == nil
}

func fieldStr(t *testing.T, res *EventResults, key string) string {
	t.Helper()

	v, ok := field(res, key)
	if !ok {
		return ""
	}

	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s is %T, want string", key, v)
	}

	return s
}

// checkValue compares got to want. want "" skips the check, isNow and isEmpty are sentinels
func checkValue(t *testing.T, label, got, want string) {
	t.Helper()

	switch want {
	case "":
		return

	case isEmpty:
		if got != "" {
			t.Errorf("%s = %q, want empty", label, got)
		}

	case isNow:
		ts, err := time.Parse(time.RFC3339, got)
		if err != nil {
			t.Errorf("%s = %q, want a time near now", label, got)
			return
		}

		if d := time.Since(ts); d < -2*time.Second || d > 2*time.Second {
			t.Errorf("%s = %q, want a time near now", label, got)
		}

	default:
		if got != want {
			t.Errorf("%s = %q, want %q", label, got, want)
		}
	}
}

type wantSchedule struct {
	status, source, primary, backup string
	matched                         bool
}

func assertSchedule(t *testing.T, res *EventResults, w wantSchedule) {
	t.Helper()

	matched, _ := field(res, "schedule.matched")
	if matched != w.matched {
		t.Errorf("schedule.matched = %v, want %t", matched, w.matched)
	}

	checkValue(t, "schedule.routingSource", fieldStr(t, res, "schedule.routingSource"), w.source)
	checkValue(t, "schedule.status", fieldStr(t, res, "schedule.status"), w.status)
	checkValue(t, "schedule.primary", fieldStr(t, res, "schedule.primary"), w.primary)
	checkValue(t, "schedule.backup", fieldStr(t, res, "schedule.backup"), w.backup)
}

type wantNotif struct {
	tag     string // previous | current
	status  string
	source  string
	summary string // Error | Correction
	start   string
	end     string
}

func assertNotifications(t *testing.T, got []notify.Notification, want []wantNotif) {
	t.Helper()
	assertNotificationsFor(t, "BUS1", got, want)
}

func assertNotificationsFor(t *testing.T, bus string, got []notify.Notification, want []wantNotif) {
	t.Helper()
	assertNotificationsOf(t, bus, Notification, got, want)
}

// assertNotificationsOf checks notifications produced by an event of the given type
func assertNotificationsOf(t *testing.T, bus string, eventType EventType, got []notify.Notification, want []wantNotif) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("notifications = %d, want %d", len(got), len(want))
	}

	for i, w := range want {
		n := got[i]

		if len(n.Classification.Tags) != 1 || n.Classification.Tags[0] != w.tag {
			t.Errorf("notification[%d] tags = %v, want [%s]", i, n.Classification.Tags, w.tag)
		}

		if !strings.Contains(n.Message.Summary, w.summary) {
			t.Errorf("notification[%d] summary = %q, want %s", i, n.Message.Summary, w.summary)
		}

		if len(n.Context.Basic) != 1 {
			t.Fatalf("notification[%d] basic context = %d, want 1", i, len(n.Context.Basic))
		}

		d := n.Context.Basic[0].Details

		checkValue(t, "notification status", d.Status, w.status)
		checkValue(t, "notification source", d.Source, w.source)
		checkValue(t, "notification start", d.Start, w.start)
		checkValue(t, "notification end", d.End, w.end)

		if d.Busname != bus || d.EventType != eventType.String() {
			t.Errorf("notification[%d] busname/eventType = %q/%q", i, d.Busname, d.EventType)
		}
	}
}

func assertCounters(t *testing.T, got *Counters, want Counters) {
	t.Helper()

	if *got != want {
		t.Errorf("counters = %+v, want %+v", *got, want)
	}
}

func assertBus(t *testing.T, dst string, state RoutingState, src string) *BusState {
	t.Helper()

	bs, ok := busCache.Get(dst)
	if !ok {
		t.Fatalf("busCache has no entry for %s", dst)
	}

	if bs.State != state || bs.Source != src {
		t.Errorf("bus %s = %s/%q, want %s/%q", dst, bs.State, bs.Source, state, src)
	}

	return bs
}

// seedSchedule stores an active ES routing for BUS1 (Pri SRC1, Sec SRC2)
func seedSchedule() {
	now := time.Now()
	scheduleCache.Set("BUS1", []*insite.BusRouting{routing("SRC1", "SRC2", now.Add(-time.Hour), now.Add(time.Hour))})
}

func TestCreateEventFromEdgeBuild(t *testing.T) {
	t.Run("tag must match exactly", func(t *testing.T) {
		resetCaches()

		res, err := newTestBeat(t, beatOpts{}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{tags: []string{"MES-2"}, src: "SRC1"}), "MES", &Counters{}, Query)

		if res != nil || !errors.Is(err, ErrEdgeTagNotFound) {
			t.Fatalf("got %v %v, want ErrEdgeTagNotFound", res, err)
		}
	})

	t.Run("base fields", func(t *testing.T) {
		resetCaches()
		seedSchedule()

		res, err := newTestBeat(t, beatOpts{}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{tags: []string{"MES", "OTHER"}, src: "SRC1"}), "MES", &Counters{}, Query)
		if err != nil {
			t.Fatal(err)
		}

		for key, want := range map[string]string{
			"dstId":                "id-BUS1",
			"dstName":              "BUS1",
			"dstTag":               "MES",
			"dstType":              "dst",
			"eventType":            "query",
			"dstNameset.broadview": "BUS1",
			"destinationLabel":     "BUS1",
			"sourceLabel":          "SRC1",
			"routeableTerminal.subscribedSource.name":              "SRC1",
			"routeableTerminal.subscribedSource.nameset.broadview": "SRC1",
			"schedule.buscode": "BUS1",
		} {
			checkValue(t, key, fieldStr(t, res, key), want)
		}

		if tags, _ := field(res, "dstTags"); !reflect.DeepEqual(tags, []string{"MES", "OTHER"}) {
			t.Errorf("dstTags = %v", tags)
		}
	})

	t.Run("mapping disabled", func(t *testing.T) {
		resetCaches()
		seedSchedule()

		res, err := newTestBeat(t, beatOpts{noMapping: true}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{src: "SRC1"}), "MES", &Counters{}, Query)

		// without mapping there's no destination label to look up
		if !errors.Is(err, ErrScheduleBusNotFound) {
			t.Fatalf("err = %v, want ErrScheduleBusNotFound", err)
		}

		for _, key := range []string{"destinationLabel", "sourceLabel"} {
			if _, ok := field(res, key); ok {
				t.Errorf("%s should not be set without mapping", key)
			}
		}
	})

	t.Run("destination nameset missing uses the default label", func(t *testing.T) {
		resetCaches()

		res, _ := newTestBeat(t, beatOpts{defLabel: "DEFAULT"}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{src: "SRC1", noDstNameset: true}), "MES", &Counters{}, Query)

		checkValue(t, "destinationLabel", fieldStr(t, res, "destinationLabel"), "DEFAULT")
	})

	t.Run("physical source only", func(t *testing.T) {
		resetCaches()
		now := time.Now()
		scheduleCache.Set("BUS1", []*insite.BusRouting{routing("PHY1", "", now.Add(-time.Hour), now.Add(time.Hour))})

		res, err := newTestBeat(t, beatOpts{}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{physical: "PHY1"}), "MES", &Counters{}, Query)
		if err != nil {
			t.Fatal(err)
		}

		checkValue(t, "physicalSource.name", fieldStr(t, res, "routeableTerminal.physicalSource.name"), "PHY1")
		checkValue(t, "physicalSource.nameset", fieldStr(t, res, "routeableTerminal.physicalSource.nameset.broadview"), "PHY1")
		checkValue(t, "sourceLabel", fieldStr(t, res, "sourceLabel"), "PHY1")

		// only the subscribed source is matched against the schedule, so the source label is blank
		checkValue(t, "schedule.status", fieldStr(t, res, "schedule.status"), Unknown.String())
	})

	t.Run("subscribed source overrides the physical source label", func(t *testing.T) {
		resetCaches()
		seedSchedule()

		res, _ := newTestBeat(t, beatOpts{}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{physical: "PHY1", src: "SRC1"}), "MES", &Counters{}, Query)

		checkValue(t, "sourceLabel", fieldStr(t, res, "sourceLabel"), "SRC1")
		checkValue(t, "physicalSource.name", fieldStr(t, res, "routeableTerminal.physicalSource.name"), "PHY1")
	})

	t.Run("no sources removes routeableTerminal", func(t *testing.T) {
		resetCaches()
		seedSchedule()

		res, _ := newTestBeat(t, beatOpts{defLabel: "DEFAULT"}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{}), "MES", &Counters{}, Query)

		if _, ok := field(res, "routeableTerminal"); ok {
			t.Error("routeableTerminal should be removed")
		}

		checkValue(t, "sourceLabel", fieldStr(t, res, "sourceLabel"), "DEFAULT")
	})
}

func TestCreateEventFromEdgeScheduleLookup(t *testing.T) {
	now := time.Now()
	active := func(pri, sec string) *insite.BusRouting {
		return routing(pri, sec, now.Add(-time.Hour), now.Add(time.Hour))
	}
	expired := func(pri, sec string) *insite.BusRouting {
		return routing(pri, sec, now.Add(-2*time.Hour), now.Add(-time.Hour))
	}

	t.Run("not found", func(t *testing.T) {
		resetCaches()
		counters := &Counters{}

		res, err := newTestBeat(t, beatOpts{webhook: true}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{src: "SRC1"}), "MES", counters, Query)

		if !errors.Is(err, ErrScheduleBusNotFound) {
			t.Fatalf("err = %v, want ErrScheduleBusNotFound", err)
		}

		assertSchedule(t, res, wantSchedule{matched: false, source: "none"})
		assertCounters(t, counters, Counters{})

		if busCache.Has("BUS1") {
			t.Error("busCache entry should not be created for an unmatched bus")
		}
	})

	tests := []struct {
		name  string
		opts  beatOpts
		es    *insite.BusRouting
		hot   *insite.BusRouting
		found bool
		want  wantSchedule
	}{
		{
			name: "expired ES entry with end date validation",
			es:   expired("SRC1", "SRC2"),
		},
		{
			name:  "expired ES entry without end date validation",
			opts:  beatOpts{noValidate: true},
			es:    expired("SRC1", "SRC2"),
			found: true,
			want:  wantSchedule{matched: true, source: "cache", status: "Primary", primary: "SRC1", backup: "SRC2"},
		},
		{
			name:  "active ES entry",
			es:    active("SRC1", "SRC2"),
			found: true,
			want:  wantSchedule{matched: true, source: "cache", status: "Primary", primary: "SRC1", backup: "SRC2"},
		},
		{
			name:  "active hot entry with webhook enabled",
			opts:  beatOpts{webhook: true},
			es:    active("ES_PRI", "ES_SEC"),
			hot:   active("SRC1", ""),
			found: true,
			want:  wantSchedule{matched: true, source: "hotcache", status: "Primary", primary: "SRC1", backup: isEmpty},
		},
		{
			name:  "active hot entry ignored when webhook and events api are off",
			es:    active("ES_PRI", "SRC1"),
			hot:   active("SRC1", ""),
			found: true,
			want:  wantSchedule{matched: true, source: "cache", status: "Backup", primary: "ES_PRI", backup: "SRC1"},
		},
		{
			name:  "expired hot entry falls back to ES",
			opts:  beatOpts{webhook: true},
			es:    active("ES_PRI", "SRC1"),
			hot:   expired("SRC1", ""),
			found: true,
			want:  wantSchedule{matched: true, source: "cache", status: "Backup", primary: "ES_PRI", backup: "SRC1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCaches()

			if tt.es != nil {
				scheduleCache.Set("BUS1", []*insite.BusRouting{tt.es})
			}
			if tt.hot != nil {
				scheduleHotCache.Set("BUS1", tt.hot)
			}

			res, err := newTestBeat(t, tt.opts).CreateEventFromEdge(
				newTestEdge("BUS1", edgeOpts{src: "SRC1"}), "MES", &Counters{}, Query)

			if !tt.found {
				if !errors.Is(err, ErrScheduleBusNotFound) {
					t.Fatalf("err = %v, want ErrScheduleBusNotFound", err)
				}
				assertSchedule(t, res, wantSchedule{matched: false, source: "none"})
				return
			}

			if err != nil {
				t.Fatal(err)
			}

			assertSchedule(t, res, tt.want)
		})
	}
}

func TestCreateEventFromEdgeStateResolution(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		srcTags []string
		want    RoutingState
	}{
		{name: "primary", src: "SRC1", want: Primary},
		{name: "backup", src: "SRC2", want: Backup},
		{name: "zorro", src: "OTHER", srcTags: []string{"zorro"}, want: Zorro},
		{name: "tda", src: "OTHER", srcTags: []string{"tda"}, want: TDA},
		{name: "unscheduled", src: "OTHER", want: Unsched},
		{name: "label match beats source tag", src: "SRC1", srcTags: []string{"zorro"}, want: Primary},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCaches()
			seedSchedule()
			counters := &Counters{}

			res, err := newTestBeat(t, beatOpts{}).CreateEventFromEdge(
				newTestEdge("BUS1", edgeOpts{src: tt.src, srcTags: tt.srcTags}), "MES", counters, Query)
			if err != nil {
				t.Fatal(err)
			}

			assertSchedule(t, res, wantSchedule{matched: true, source: "cache", status: tt.want.String(), primary: "SRC1", backup: "SRC2"})

			want := Counters{}
			want.Increment(tt.want)
			assertCounters(t, counters, want)

			assertBus(t, "BUS1", tt.want, tt.src)

			if len(res.notifications) != 0 {
				t.Errorf("query should not build notifications, got %d", len(res.notifications))
			}
		})
	}
}

// a blank source label reports the unknown status only. the stored bus state, notifications
// and counters are left alone
func TestCreateEventFromEdgeBlankSource(t *testing.T) {
	useNotifier(t, stubNotifier{})

	tests := []struct {
		name      string
		opts      beatOpts
		edge      edgeOpts
		eventType EventType
		seed      bool // seed BUS1 as Backup on SRC2 with a transition 10 minutes ago
	}{
		{name: "query on a new bus", edge: edgeOpts{}, eventType: Query},
		{name: "query keeps the stored state", edge: edgeOpts{}, eventType: Query, seed: true},
		{name: "notification keeps the stored state", edge: edgeOpts{}, eventType: Notification, seed: true},
		{name: "physical source only", edge: edgeOpts{physical: "PHY1"}, eventType: Notification, seed: true},
		{name: "subscribed source without a broadview nameset", edge: edgeOpts{src: "SRC1", noSrcNameset: true}, eventType: Notification, seed: true},
		{name: "zorro tag does not apply without a label", edge: edgeOpts{src: "ZSRC", srcTags: []string{"zorro"}, noSrcNameset: true}, eventType: Notification, seed: true},
		{name: "no recheck with the hot cache enabled", opts: beatOpts{webhook: true}, edge: edgeOpts{}, eventType: Notification, seed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCaches()
			seedSchedule()

			var seeded BusState
			if tt.seed {
				seeded = *seedBus("BUS1", Backup, "SRC2", 10*time.Minute, 0, 0)
			}

			counters := &Counters{}
			began := time.Now()

			res, err := newTestBeat(t, tt.opts).CreateEventFromEdge(newTestEdge("BUS1", tt.edge), "MES", counters, tt.eventType)
			if err != nil {
				t.Fatal(err)
			}

			if elapsed := time.Since(began); elapsed >= testWait {
				t.Errorf("took %s, expected no recheck wait", elapsed)
			}

			assertSchedule(t, res, wantSchedule{matched: true, source: "cache", status: Unknown.String(), primary: "SRC1", backup: "SRC2"})
			assertNotifications(t, res.notifications, nil)
			assertCounters(t, counters, Counters{})

			for _, key := range []string{"schedule.deviationStartTime", "schedule.deviationEndTime"} {
				if _, ok := field(res, key); ok {
					t.Errorf("%s should not be set", key)
				}
			}

			if !tt.seed {
				if busCache.Has("BUS1") {
					t.Error("busCache entry should not be created for a blank source")
				}
				return
			}

			got := assertBus(t, "BUS1", Backup, "SRC2")
			if got.Counter != 0 || !got.Transition.Equal(*seeded.Transition) {
				t.Errorf("bus state changed: %+v", got)
			}
		})
	}
}

func TestCreateEventFromEdgeNotificationNoChange(t *testing.T) {
	useNotifier(t, stubNotifier{})

	t.Run("same state updates the source label", func(t *testing.T) {
		resetCaches()
		seedSchedule()
		seedBus("BUS1", Primary, "SRC_OLD", 0, 0, 0)
		counters := &Counters{}

		res, err := newTestBeat(t, beatOpts{}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{src: "SRC1"}), "MES", counters, Notification)
		if err != nil {
			t.Fatal(err)
		}

		assertNotifications(t, res.notifications, nil)
		assertCounters(t, counters, Counters{Primary: 1})
		assertBus(t, "BUS1", Primary, "SRC1")
		checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), "-")
	})

	t.Run("same state reports the existing transition time", func(t *testing.T) {
		resetCaches()
		seedSchedule()
		bs := seedBus("BUS1", Backup, "SRC2", 10*time.Minute, 0, 0)

		res, _ := newTestBeat(t, beatOpts{}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{src: "SRC2"}), "MES", &Counters{}, Notification)

		checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), fmtTime(bs.Transition))
	})

	t.Run("unscheduled to unscheduled does not recheck", func(t *testing.T) {
		resetCaches()
		seedSchedule()
		seedBus("BUS1", Unsched, "SRC_NEW", 10*time.Minute, 0, 0)

		srv, calls := newEventsAPIStub(t, http.StatusOK, func(string) string { return "[]" })
		began := time.Now()

		res, _ := newTestBeat(t, beatOpts{webhook: true, eventsAPI: srv.URL}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{src: "SRC_NEW"}), "MES", &Counters{}, Notification)

		if elapsed := time.Since(began); elapsed >= testWait {
			t.Errorf("took %s, expected no recheck wait", elapsed)
		}

		if calls.Load() != 0 {
			t.Errorf("events api calls = %d, want 0", calls.Load())
		}

		assertSchedule(t, res, wantSchedule{matched: true, source: "cache", status: Unsched.String()})
		assertNotifications(t, res.notifications, nil)
	})
}

func TestCreateEventFromEdgeTransitions(t *testing.T) {
	useNotifier(t, stubNotifier{})

	type seed struct {
		state                               RoutingState
		src                                 string
		transitionAgo, startAgo, restoreAgo time.Duration
	}

	tests := []struct {
		name      string
		seed      *seed // nil = new bus (Unknown)
		opts      beatOpts
		edge      edgeOpts
		want      wantSchedule
		notifs    func(bs *BusState) []wantNotif // seeded state passed in for expected times
		counters  Counters
		bus       RoutingState
		devStart  func(bs *BusState) string
		devEnd    string
		noRecheck bool // assert no wait
		apiStub   bool // enable a stub events api and assert it isn't called
	}{
		{
			name: "new bus to backup",
			edge: edgeOpts{src: "SRC2"},
			want: wantSchedule{matched: true, source: "cache", status: "Backup"},
			notifs: func(*BusState) []wantNotif {
				return []wantNotif{{"current", "Backup", "SRC2", "Error", isNow, isEmpty}}
			},
			counters: Counters{Backup: 1},
			bus:      Backup,
			devStart: func(*BusState) string { return isNow },
		},
		{
			// pinned: a new bus that first reports Primary is treated as a restore
			name: "new bus to primary",
			edge: edgeOpts{src: "SRC1"},
			want: wantSchedule{matched: true, source: "cache", status: "Primary"},
			notifs: func(*BusState) []wantNotif {
				return []wantNotif{{"current", "Primary", "SRC1", "Correction", isNow, isEmpty}}
			},
			counters: Counters{Primary: 1},
			bus:      Primary,
			devStart: func(*BusState) string { return isNow },
			devEnd:   isNow,
		},
		{
			name: "primary to backup",
			seed: &seed{state: Primary, src: "SRC1"},
			edge: edgeOpts{src: "SRC2"},
			want: wantSchedule{matched: true, source: "cache", status: "Backup"},
			notifs: func(*BusState) []wantNotif {
				return []wantNotif{
					{"previous", "Primary", "SRC1", "Error", "-", isNow},
					{"current", "Backup", "SRC2", "Error", isNow, isEmpty},
				}
			},
			counters: Counters{Primary: -1, Backup: 1},
			bus:      Backup,
			devStart: func(*BusState) string { return isNow },
		},
		{
			name: "primary to backup after a previous restore",
			seed: &seed{state: Primary, src: "SRC1", restoreAgo: 30 * time.Minute},
			edge: edgeOpts{src: "SRC2"},
			want: wantSchedule{matched: true, source: "cache", status: "Backup"},
			notifs: func(bs *BusState) []wantNotif {
				return []wantNotif{
					{"previous", "Primary", "SRC1", "Error", fmtTime(bs.Restore), isNow},
					{"current", "Backup", "SRC2", "Error", isNow, isEmpty},
				}
			},
			counters: Counters{Primary: -1, Backup: 1},
			bus:      Backup,
			devStart: func(*BusState) string { return isNow },
		},
		{
			name: "primary to zorro",
			seed: &seed{state: Primary, src: "SRC1"},
			edge: edgeOpts{src: "ZSRC", srcTags: []string{"zorro"}},
			want: wantSchedule{matched: true, source: "cache", status: "Zorro"},
			notifs: func(*BusState) []wantNotif {
				return []wantNotif{
					{"previous", "Primary", "SRC1", "Error", "-", isNow},
					{"current", "Zorro", "ZSRC", "Error", isNow, isEmpty},
				}
			},
			counters: Counters{Primary: -1, Zorro: 1},
			bus:      Zorro,
			devStart: func(*BusState) string { return isNow },
		},
		{
			name: "primary to tda",
			seed: &seed{state: Primary, src: "SRC1"},
			edge: edgeOpts{src: "TSRC", srcTags: []string{"tda"}},
			want: wantSchedule{matched: true, source: "cache", status: "TDA"},
			notifs: func(*BusState) []wantNotif {
				return []wantNotif{
					{"previous", "Primary", "SRC1", "Error", "-", isNow},
					{"current", "TDA", "TSRC", "Error", isNow, isEmpty},
				}
			},
			counters: Counters{Primary: -1, Tda: 1},
			bus:      TDA,
			devStart: func(*BusState) string { return isNow },
		},
		{
			name: "backup to primary reports when the bus first left primary",
			seed: &seed{state: Backup, src: "SRC2", transitionAgo: 10 * time.Minute, startAgo: 20 * time.Minute},
			edge: edgeOpts{src: "SRC1"},
			want: wantSchedule{matched: true, source: "cache", status: "Primary"},
			notifs: func(bs *BusState) []wantNotif {
				return []wantNotif{
					{"previous", "Backup", "SRC2", "Correction", fmtTime(bs.Transition), isNow},
					{"current", "Primary", "SRC1", "Correction", isNow, isEmpty},
				}
			},
			counters: Counters{Backup: -1, Primary: 1},
			bus:      Primary,
			devStart: func(bs *BusState) string { return fmtTime(bs.TransitionStart) },
			devEnd:   isNow,
		},
		{
			// known quirk (pinned): on the first deviation there is no Restore, so TransitionStart is never set
			// and the restore reports the last transition time instead of when the bus left Primary
			name: "backup to primary on the first deviation reports the last transition",
			seed: &seed{state: Backup, src: "SRC2", transitionAgo: 10 * time.Minute},
			edge: edgeOpts{src: "SRC1"},
			want: wantSchedule{matched: true, source: "cache", status: "Primary"},
			notifs: func(bs *BusState) []wantNotif {
				return []wantNotif{
					{"previous", "Backup", "SRC2", "Correction", fmtTime(bs.Transition), isNow},
					{"current", "Primary", "SRC1", "Correction", isNow, isEmpty},
				}
			},
			counters: Counters{Backup: -1, Primary: 1},
			bus:      Primary,
			devStart: func(*BusState) string { return isNow },
			devEnd:   isNow,
		},
		{
			name: "primary to unscheduled with the hot cache disabled",
			seed: &seed{state: Primary, src: "SRC1"},
			edge: edgeOpts{src: "SRC_NEW"},
			want: wantSchedule{matched: true, source: "cache", status: Unsched.String(), primary: "SRC1", backup: "SRC2"},
			notifs: func(*BusState) []wantNotif {
				return []wantNotif{
					{"previous", "Primary", "SRC1", "Error", "-", isNow},
					{"current", Unsched.String(), "SRC_NEW", "Error", isNow, isEmpty},
				}
			},
			counters:  Counters{Primary: -1, Unsched: 1},
			bus:       Unsched,
			devStart:  func(*BusState) string { return isNow },
			noRecheck: true,
		},
		{
			name: "new bus to unscheduled does not recheck",
			opts: beatOpts{webhook: true},
			edge: edgeOpts{src: "SRC_NEW"},
			want: wantSchedule{matched: true, source: "cache", status: Unsched.String()},
			notifs: func(*BusState) []wantNotif {
				return []wantNotif{{"current", Unsched.String(), "SRC_NEW", "Error", isNow, isEmpty}}
			},
			counters:  Counters{Unsched: 1},
			bus:       Unsched,
			devStart:  func(*BusState) string { return isNow },
			noRecheck: true,
			apiStub:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCaches()
			seedSchedule()

			// copy of the seeded state for expected values (the cached one is mutated)
			var seeded BusState
			if tt.seed != nil {
				seeded = *seedBus("BUS1", tt.seed.state, tt.seed.src, tt.seed.transitionAgo, tt.seed.startAgo, tt.seed.restoreAgo)
			}

			opts := tt.opts

			// stub events api to prove the recheck didn't call it
			var calls *atomic.Int32
			if tt.apiStub {
				var srv *httptest.Server
				srv, calls = newEventsAPIStub(t, http.StatusOK, func(string) string { return "[]" })
				opts.eventsAPI = srv.URL
			}

			counters := &Counters{}
			began := time.Now()

			res, err := newTestBeat(t, opts).CreateEventFromEdge(newTestEdge("BUS1", tt.edge), "MES", counters, Notification)
			if err != nil {
				t.Fatal(err)
			}

			if tt.noRecheck {
				if elapsed := time.Since(began); elapsed >= testWait {
					t.Errorf("took %s, expected no recheck wait", elapsed)
				}
				if calls != nil && calls.Load() != 0 {
					t.Errorf("events api calls = %d, want 0", calls.Load())
				}
			}

			assertSchedule(t, res, tt.want)
			assertNotifications(t, res.notifications, tt.notifs(&seeded))
			assertCounters(t, counters, tt.counters)
			assertBus(t, "BUS1", tt.bus, tt.edge.src)

			checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), tt.devStart(&seeded))

			if tt.devEnd == "" {
				if _, ok := field(res, "schedule.deviationEndTime"); ok {
					t.Error("deviationEndTime should not be set")
				}
			} else {
				checkValue(t, "deviationEndTime", fieldStr(t, res, "schedule.deviationEndTime"), tt.devEnd)
			}
		})
	}
}

func TestCreateEventFromEdgeUnscheduledRecheck(t *testing.T) {
	useNotifier(t, stubNotifier{})

	type waitExpect int
	const (
		waitAny waitExpect = iota
		waits
		noWait
	)

	start, end := activeWindow()
	past := time.Now().UTC()
	pastStart, pastEnd := past.Add(-2*time.Hour).Format(schedulerDateLayout), past.Add(-time.Hour).Format(schedulerDateLayout)

	mainEvent := func(src string) func(string) string {
		return func(output string) string { return apgEventJSON(output, src, "MAIN", start, end) }
	}

	tests := []struct {
		name      string
		seed      func() *BusState
		hot       *insite.BusRouting
		webhook   bool
		apiStatus int                        // 0 = no events api
		apiBody   func(output string) string // body for a 200
		post      string                     // webhook payload delivered during the wait
		want      wantSchedule
		notifs    []wantNotif
		counters  Counters
		bus       RoutingState
		wait      waitExpect
		apiCalls  int32
		hotEmpty  bool
		devStart  func(bs *BusState) string
		devEnd    string
	}{
		{
			name:     "webhook success with no transition",
			seed:     func() *BusState { return seedBus("BUS1", Primary, "SRC_NEW", 0, 0, 0) },
			webhook:  true,
			post:     apgEventJSON("BUS1", "SRC_NEW", "MAIN", start, end),
			want:     wantSchedule{matched: true, source: "hotcache", status: "Primary", primary: "SRC_NEW", backup: isEmpty},
			counters: Counters{Primary: 1},
			bus:      Primary,
		},
		{
			name:    "webhook success as backup is a transition",
			seed:    func() *BusState { return seedBus("BUS1", Primary, "SRC1", 0, 0, 0) },
			webhook: true,
			post:    apgEventJSON("BUS1", "SRC_NEW", "BACKUP", start, end),
			want:    wantSchedule{matched: true, source: "hotcache", status: "Backup", primary: isEmpty, backup: "SRC_NEW"},
			notifs: []wantNotif{
				{"previous", "Primary", "SRC1", "Error", "-", isNow},
				{"current", "Backup", "SRC_NEW", "Error", isNow, isEmpty},
			},
			counters: Counters{Primary: -1, Backup: 1},
			bus:      Backup,
		},
		{
			name:      "events api success with a correction",
			seed:      func() *BusState { return seedBus("BUS1", Backup, "SRC2", 10*time.Minute, 20*time.Minute, 0) },
			apiStatus: http.StatusOK,
			apiBody:   mainEvent("SRC_NEW"),
			want:      wantSchedule{matched: true, source: "hotcache", status: "Primary", primary: "SRC_NEW", backup: isEmpty},
			notifs: []wantNotif{
				{"previous", "Backup", "SRC2", "Correction", "", isNow},
				{"current", "Primary", "SRC_NEW", "Correction", isNow, isEmpty},
			},
			counters: Counters{Backup: -1, Primary: 1},
			bus:      Primary,
			wait:     noWait,
			apiCalls: 1,
			devStart: func(bs *BusState) string { return fmtTime(bs.TransitionStart) },
			devEnd:   isNow,
		},
		{
			name:      "events api success returns before the webhook wait",
			seed:      func() *BusState { return seedBus("BUS1", Primary, "SRC1", 0, 0, 0) },
			webhook:   true,
			apiStatus: http.StatusOK,
			apiBody:   mainEvent("SRC_NEW"),
			want:      wantSchedule{matched: true, source: "hotcache", status: "Primary", primary: "SRC_NEW"},
			counters:  Counters{Primary: 1},
			bus:       Primary,
			wait:      noWait,
			apiCalls:  1,
		},
		{
			name:    "webhook only failure",
			seed:    func() *BusState { return seedBus("BUS1", Primary, "SRC1", 0, 0, 0) },
			webhook: true,
			want:    wantSchedule{matched: true, source: "cache", status: Unsched.String(), primary: "SRC1", backup: "SRC2"},
			notifs: []wantNotif{
				{"previous", "Primary", "SRC1", "Error", "-", isNow},
				{"current", Unsched.String(), "SRC_NEW", "Error", isNow, isEmpty},
			},
			counters: Counters{Primary: -1, Unsched: 1},
			bus:      Unsched,
			wait:     waits,
			hotEmpty: true,
		},
		{
			name:      "events api error without webhook",
			seed:      func() *BusState { return seedBus("BUS1", Primary, "SRC1", 0, 0, 0) },
			apiStatus: http.StatusInternalServerError,
			want:      wantSchedule{matched: true, source: "cache", status: Unsched.String(), primary: "SRC1", backup: "SRC2"},
			notifs: []wantNotif{
				{"previous", "Primary", "SRC1", "Error", "-", isNow},
				{"current", Unsched.String(), "SRC_NEW", "Error", isNow, isEmpty},
			},
			counters: Counters{Primary: -1, Unsched: 1},
			bus:      Unsched,
			wait:     noWait,
			apiCalls: 1,
			hotEmpty: true,
		},
		{
			name:      "events api error falls back to the webhook",
			seed:      func() *BusState { return seedBus("BUS1", Primary, "SRC1", 0, 0, 0) },
			webhook:   true,
			apiStatus: http.StatusInternalServerError,
			post:      apgEventJSON("BUS1", "SRC_NEW", "MAIN", start, end),
			want:      wantSchedule{matched: true, source: "hotcache", status: "Primary", primary: "SRC_NEW"},
			counters:  Counters{Primary: 1},
			bus:       Primary,
			apiCalls:  1,
		},
		{
			name:      "events api returns a different source",
			seed:      func() *BusState { return seedBus("BUS1", Primary, "SRC1", 0, 0, 0) },
			webhook:   true,
			apiStatus: http.StatusOK,
			apiBody:   mainEvent("OTHER_SRC"),
			want:      wantSchedule{matched: true, source: "cache", status: Unsched.String(), primary: "SRC1", backup: "SRC2"},
			notifs: []wantNotif{
				{"previous", "Primary", "SRC1", "Error", "-", isNow},
				{"current", Unsched.String(), "SRC_NEW", "Error", isNow, isEmpty},
			},
			counters: Counters{Primary: -1, Unsched: 1},
			bus:      Unsched,
			wait:     waits,
			apiCalls: 1,
		},
		{
			name:      "events api returns an inactive window",
			seed:      func() *BusState { return seedBus("BUS1", Primary, "SRC1", 0, 0, 0) },
			apiStatus: http.StatusOK,
			apiBody: func(output string) string {
				return apgEventJSON(output, "SRC_NEW", "MAIN", pastStart, pastEnd)
			},
			want:     wantSchedule{matched: true, source: "cache", status: Unsched.String()},
			notifs:   []wantNotif{{"previous", "Primary", "SRC1", "Error", "-", isNow}, {"current", Unsched.String(), "SRC_NEW", "Error", isNow, isEmpty}},
			counters: Counters{Primary: -1, Unsched: 1},
			bus:      Unsched,
			wait:     noWait,
			apiCalls: 1,
			hotEmpty: true,
		},
		{
			name:      "events api returns untagged and non-apg events",
			seed:      func() *BusState { return seedBus("BUS1", Primary, "SRC1", 0, 0, 0) },
			apiStatus: http.StatusOK,
			apiBody: func(output string) string {
				return "[" + apgEventJSON(output, "SRC_NEW", "OTHER", start, end) + `,{"task":"router","output":"` + output + `"}]`
			},
			want:     wantSchedule{matched: true, source: "cache", status: Unsched.String()},
			notifs:   []wantNotif{{"previous", "Primary", "SRC1", "Error", "-", isNow}, {"current", Unsched.String(), "SRC_NEW", "Error", isNow, isEmpty}},
			counters: Counters{Primary: -1, Unsched: 1},
			bus:      Unsched,
			wait:     noWait,
			apiCalls: 1,
			hotEmpty: true,
		},
		{
			// the hot entry wins the lookup but doesn't match the source, so the routing stays the hot one
			name:     "hot entry present for a different source",
			seed:     func() *BusState { return seedBus("BUS1", Primary, "OTHER_SRC", 0, 0, 0) },
			hot:      routing("OTHER_SRC", "", time.Now().Add(-time.Minute), time.Now().Add(time.Hour)),
			webhook:  true,
			want:     wantSchedule{matched: true, source: "hotcache", status: Unsched.String(), primary: "OTHER_SRC", backup: isEmpty},
			notifs:   []wantNotif{{"previous", "Primary", "OTHER_SRC", "Error", "-", isNow}, {"current", Unsched.String(), "SRC_NEW", "Error", isNow, isEmpty}},
			counters: Counters{Primary: -1, Unsched: 1},
			bus:      Unsched,
			wait:     waits,
		},
		{
			name:     "expired hot entry for the source",
			seed:     func() *BusState { return seedBus("BUS1", Primary, "SRC1", 0, 0, 0) },
			hot:      routing("SRC_NEW", "", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)),
			webhook:  true,
			want:     wantSchedule{matched: true, source: "cache", status: Unsched.String(), primary: "SRC1", backup: "SRC2"},
			notifs:   []wantNotif{{"previous", "Primary", "SRC1", "Error", "-", isNow}, {"current", Unsched.String(), "SRC_NEW", "Error", isNow, isEmpty}},
			counters: Counters{Primary: -1, Unsched: 1},
			bus:      Unsched,
			wait:     waits,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetCaches()
			seedSchedule()

			seeded := *tt.seed()

			if tt.hot != nil {
				scheduleHotCache.Set("BUS1", tt.hot)
			}

			opts := beatOpts{webhook: tt.webhook}

			var calls *atomic.Int32
			if tt.apiStatus != 0 {
				var srv *httptest.Server
				srv, calls = newEventsAPIStub(t, tt.apiStatus, tt.apiBody)
				opts.eventsAPI = srv.URL
			}

			if tt.post != "" {
				postWebhookAfter(10*time.Millisecond, tt.post)
			}

			counters := &Counters{}
			began := time.Now()

			res, err := newTestBeat(t, opts).CreateEventFromEdge(
				newTestEdge("BUS1", edgeOpts{src: "SRC_NEW"}), "MES", counters, Notification)
			if err != nil {
				t.Fatal(err)
			}

			elapsed := time.Since(began)

			switch tt.wait {
			case waits:
				if elapsed < testWait {
					t.Errorf("took %s, expected the webhook wait", elapsed)
				}
			case noWait:
				if elapsed >= testWait {
					t.Errorf("took %s, expected no wait", elapsed)
				}
			}

			if calls != nil && calls.Load() != tt.apiCalls {
				t.Errorf("events api calls = %d, want %d", calls.Load(), tt.apiCalls)
			}

			if tt.hotEmpty && scheduleHotCache.Length() != 0 {
				t.Errorf("hot cache should be empty, has %d entries", scheduleHotCache.Length())
			}

			assertSchedule(t, res, tt.want)
			assertNotifications(t, res.notifications, tt.notifs)
			assertCounters(t, counters, tt.counters)
			assertBus(t, "BUS1", tt.bus, "SRC_NEW")

			if tt.devStart != nil {
				checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), tt.devStart(&seeded))
			}

			if tt.devEnd != "" {
				checkValue(t, "deviationEndTime", fieldStr(t, res, "schedule.deviationEndTime"), tt.devEnd)
			}
		})
	}
}

func TestCreateEventFromEdgeQueryDefunct(t *testing.T) {
	useNotifier(t, stubNotifier{})

	t.Run("backup corrected to primary on the fourth query", func(t *testing.T) {
		resetCaches()
		seedSchedule()
		bs := seedBus("BUS1", Backup, "SRC2", 10*time.Minute, 0, 0)
		transition := fmtTime(bs.Transition)

		bt := newTestBeat(t, beatOpts{})

		for call := 1; call <= 3; call++ {
			counters := &Counters{}

			res, err := bt.CreateEventFromEdge(newTestEdge("BUS1", edgeOpts{src: "SRC1"}), "MES", counters, Query)
			if err != nil {
				t.Fatal(err)
			}

			got := assertBus(t, "BUS1", Backup, "SRC2")
			if got.Counter != call {
				t.Errorf("call %d: counter = %d", call, got.Counter)
			}

			checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), transition)
			assertNoField(t, res, "schedule.deviationEndTime")
			assertCounters(t, counters, Counters{Primary: 1})
			assertNotifications(t, res.notifications, nil)
		}

		res, _ := bt.CreateEventFromEdge(newTestEdge("BUS1", edgeOpts{src: "SRC1"}), "MES", &Counters{}, Query)

		got := assertBus(t, "BUS1", Primary, "SRC1")
		if got.Transition != nil || got.Restore == nil || got.Counter != 0 {
			t.Errorf("bus after correction = %+v, want a reset transition", got)
		}

		// the event reports the deviation window: no transition start was stored, so it began at the transition
		checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), transition)
		checkValue(t, "deviationEndTime", fieldStr(t, res, "schedule.deviationEndTime"), isNow)

		// the correction closes the stored backup state and opens primary for downstream systems
		assertNotificationsOf(t, "BUS1", Query, res.notifications, []wantNotif{
			{tag: "previous", status: Backup.String(), source: "SRC2", summary: "Correction", start: transition, end: isNow},
			{tag: "current", status: Primary.String(), source: "SRC1", summary: "Correction", start: isNow, end: isEmpty},
		})
		assertDefunctTrigger(t, res.notifications)

		// the next query is in sync and doesn't notify again or report the deviation
		res, _ = bt.CreateEventFromEdge(newTestEdge("BUS1", edgeOpts{src: "SRC1"}), "MES", &Counters{}, Query)
		assertNotifications(t, res.notifications, nil)
		checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), "-")
		assertNoField(t, res, "schedule.deviationEndTime")
	})

	t.Run("correction to primary reports when the bus first left primary", func(t *testing.T) {
		resetCaches()
		seedSchedule()
		bs := seedBus("BUS1", Backup, "SRC2", 10*time.Minute, 20*time.Minute, 0)
		firstDeviation := fmtTime(bs.TransitionStart)

		bt := newTestBeat(t, beatOpts{})

		var res *EventResults
		for range 4 {
			res, _ = bt.CreateEventFromEdge(newTestEdge("BUS1", edgeOpts{src: "SRC1"}), "MES", &Counters{}, Query)
		}

		assertBus(t, "BUS1", Primary, "SRC1")
		checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), firstDeviation)
		checkValue(t, "deviationEndTime", fieldStr(t, res, "schedule.deviationEndTime"), isNow)
	})

	t.Run("primary corrected to backup on the fourth query", func(t *testing.T) {
		resetCaches()
		seedSchedule()
		seedBus("BUS1", Primary, "SRC1", 0, 0, 0)

		bt := newTestBeat(t, beatOpts{})

		var res *EventResults
		for call := 1; call <= 4; call++ {
			res, _ = bt.CreateEventFromEdge(newTestEdge("BUS1", edgeOpts{src: "SRC2"}), "MES", &Counters{}, Query)

			if call < 4 {
				assertNotifications(t, res.notifications, nil)
			}
		}

		got := assertBus(t, "BUS1", Backup, "SRC2")
		if got.Transition == nil || got.Counter != 0 {
			t.Errorf("bus after correction = %+v, want a transition time", got)
		}

		checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), isNow)

		// a correction away from primary starts a deviation, it doesn't end one
		assertNoField(t, res, "schedule.deviationEndTime")

		// primary had no transition, so the previous state start is "-"
		assertNotificationsOf(t, "BUS1", Query, res.notifications, []wantNotif{
			{tag: "previous", status: Primary.String(), source: "SRC1", summary: "Error", start: "-", end: isNow},
			{tag: "current", status: Backup.String(), source: "SRC2", summary: "Error", start: isNow, end: isEmpty},
		})
		assertDefunctTrigger(t, res.notifications)
	})

	t.Run("same state reports the transition time", func(t *testing.T) {
		resetCaches()
		seedSchedule()
		bs := seedBus("BUS1", Backup, "SRC2", 10*time.Minute, 0, 0)

		res, _ := newTestBeat(t, beatOpts{}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{src: "SRC2"}), "MES", &Counters{}, Query)

		checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), fmtTime(bs.Transition))
		assertBus(t, "BUS1", Backup, "SRC2")
	})

	t.Run("same primary state with no transition", func(t *testing.T) {
		resetCaches()
		seedSchedule()
		seedBus("BUS1", Primary, "SRC1", 0, 0, 0)

		res, _ := newTestBeat(t, beatOpts{}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{src: "SRC1"}), "MES", &Counters{}, Query)

		checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), "-")
	})

	t.Run("query does not recheck the hot cache", func(t *testing.T) {
		resetCaches()
		seedSchedule()
		seedBus("BUS1", Primary, "SRC1", 0, 0, 0)

		srv, calls := newEventsAPIStub(t, http.StatusOK, func(string) string { return "[]" })
		began := time.Now()

		res, _ := newTestBeat(t, beatOpts{webhook: true, eventsAPI: srv.URL}).CreateEventFromEdge(
			newTestEdge("BUS1", edgeOpts{src: "SRC_NEW"}), "MES", &Counters{}, Query)

		if elapsed := time.Since(began); elapsed >= testWait || calls.Load() != 0 {
			t.Errorf("took %s with %d api calls, expected no recheck", elapsed, calls.Load())
		}

		checkValue(t, "schedule.status", fieldStr(t, res, "schedule.status"), Unsched.String())
	})
}

func assertNoField(t *testing.T, res *EventResults, key string) {
	t.Helper()

	if v, ok := field(res, key); ok {
		t.Errorf("%s = %v, should not be set", key, v)
	}
}

func assertDefunctTrigger(t *testing.T, got []notify.Notification) {
	t.Helper()

	for i, n := range got {
		if trigger := n.Context.Basic[0].Details.Trigger; !strings.HasPrefix(trigger, "GraphQL Query (Defunct Correction)") {
			t.Errorf("notification[%d] trigger = %q", i, trigger)
		}
	}
}

func TestCreateEventFromEdgeNoopNotifier(t *testing.T) {
	useNotifier(t, noopNotifier{})

	resetCaches()
	seedSchedule()
	seedBus("BUS1", Primary, "SRC1", 0, 0, 0)
	counters := &Counters{}

	res, err := newTestBeat(t, beatOpts{}).CreateEventFromEdge(
		newTestEdge("BUS1", edgeOpts{src: "SRC2"}), "MES", counters, Notification)
	if err != nil {
		t.Fatal(err)
	}

	assertNotifications(t, res.notifications, []wantNotif{
		{"previous", "Primary", "SRC1", "Error", "-", isNow},
		{"current", "Backup", "SRC2", "Error", isNow, isEmpty},
	})

	for _, n := range res.notifications {
		if n.Origin.Detector.Host != "" {
			t.Errorf("origin host = %q, want empty", n.Origin.Detector.Host)
		}
	}

	assertCounters(t, counters, Counters{Primary: -1, Backup: 1})
	assertBus(t, "BUS1", Backup, "SRC2")
	checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), isNow)

	if err := (noopNotifier{}).Send(res.notifications...); err != nil {
		t.Errorf("noop send = %v", err)
	}
}

func TestCreateEventFromEdgeSequence(t *testing.T) {
	useNotifier(t, stubNotifier{})

	resetCaches()
	seedSchedule()
	bs := seedBus("BUS1", Primary, "SRC1", 0, 0, 30*time.Minute)
	restore := fmtTime(bs.Restore)

	bt := newTestBeat(t, beatOpts{})

	notification := func(src string) *EventResults {
		t.Helper()

		res, err := bt.CreateEventFromEdge(newTestEdge("BUS1", edgeOpts{src: src}), "MES", &Counters{}, Notification)
		if err != nil {
			t.Fatal(err)
		}

		return res
	}

	// shift moves the transition times back so each step is distinguishable at second resolution
	shift := func(transitionAgo, startAgo time.Duration) {
		busCache.DoMut("BUS1", func(v *BusState) {
			v.Transition = ago(transitionAgo)
			if startAgo != 0 {
				v.TransitionStart = ago(startAgo)
			}
		})
	}

	// step 1: Primary -> Backup. leaving Primary after a restore sets the TransitionStart
	res := notification("SRC2")
	assertNotifications(t, res.notifications, []wantNotif{
		{"previous", "Primary", "SRC1", "Error", restore, isNow},
		{"current", "Backup", "SRC2", "Error", isNow, isEmpty},
	})

	if got, _ := busCache.Get("BUS1"); got.TransitionStart == nil {
		t.Fatal("TransitionStart should be set when leaving Primary after a restore")
	}

	shift(20*time.Minute, 20*time.Minute)
	departed, _ := busCache.Get("BUS1")
	departure := fmtTime(departed.TransitionStart)
	backupAt := fmtTime(departed.Transition)

	// step 2: Backup -> Unsched (hot cache disabled, no recheck). TransitionStart is kept
	res = notification("SRC_NEW")
	assertNotifications(t, res.notifications, []wantNotif{
		{"previous", "Backup", "SRC2", "Error", backupAt, isNow},
		{"current", Unsched.String(), "SRC_NEW", "Error", isNow, isEmpty},
	})

	shift(10*time.Minute, 0)
	unschedAt, _ := busCache.Get("BUS1")
	unschedStart := fmtTime(unschedAt.Transition)

	// step 3: Unsched -> Primary reports when the bus first left Primary
	res = notification("SRC1")
	assertNotifications(t, res.notifications, []wantNotif{
		{"previous", Unsched.String(), "SRC_NEW", "Correction", unschedStart, isNow},
		{"current", "Primary", "SRC1", "Correction", isNow, isEmpty},
	})

	checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), departure)
	checkValue(t, "deviationEndTime", fieldStr(t, res, "schedule.deviationEndTime"), isNow)

	got := assertBus(t, "BUS1", Primary, "SRC1")
	if got.Transition != nil || got.TransitionStart != nil || got.Restore == nil {
		t.Errorf("bus after restore = %+v", got)
	}

	// step 4: a follow-up query has no deviation
	res, _ = bt.CreateEventFromEdge(newTestEdge("BUS1", edgeOpts{src: "SRC1"}), "MES", &Counters{}, Query)
	checkValue(t, "deviationStartTime", fieldStr(t, res, "schedule.deviationStartTime"), "-")
	assertNotifications(t, res.notifications, nil)
}
