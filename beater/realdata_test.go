package beater

// Tests using real lab data snapshotted into testdata/ by testdata/fetch_fixtures.py.
// the scheduler event fixtures are tested in the schedule package

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thetherington/routebeat/beater/schedule"
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

// seedRouting stores the document's schedule routing as an active routing
func (d rbDoc) seedRouting() {
	now := time.Now()
	setSchedule(map[string]*schedule.BusRouting{
		d.Schedule.Buscode: routing(d.Schedule.Primary, d.Schedule.Backup, now.Add(-time.Hour), now.Add(time.Hour)),
	})
}

func newRealBeat() *routebeat {
	// the routebeat documents store nameset keys lowercased
	return &routebeat{config: routeCfg.Config{
		Mapping:  &routeCfg.Mapping{Nameset: "broadview"},
		Zorro:    realZorroTag,
		TDA:      realTDATag,
		Schedule: routeCfg.Schedule{Dev: routeCfg.Developer{ValidateEndDate: true}},
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
