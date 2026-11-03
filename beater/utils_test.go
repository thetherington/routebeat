package beater

import (
	"slices"
	"strings"
	"testing"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"
)

// recordingClient is a beat.Client that keeps the published events
type recordingClient struct{ events []beat.Event }

func (c *recordingClient) Publish(e beat.Event)      { c.events = append(c.events, e) }
func (c *recordingClient) PublishAll(e []beat.Event) { c.events = append(c.events, e...) }
func (c *recordingClient) Close() error              { return nil }

func TestBuildEventsProcessResultsLogs(t *testing.T) {
	if err := logp.DevelopmentSetup(logp.ToObserverOutput(), logp.WithSelectors("ProcessResults.*")); err != nil {
		t.Fatal(err)
	}

	useNotifier(t, stubNotifier{})

	// BUS1 is matched, BUS9 isn't in the schedule, BUS7 is discarded (tag doesn't match exactly)
	edges := func() []Edge {
		matched := newTestEdge("BUS1", edgeOpts{src: "SRC1"})
		matched.Name = "SUB-1" // the log uses the destination label, not the magnum name

		return []Edge{*matched, *newTestEdge("BUS9", edgeOpts{src: "SRC1"}), *newTestEdge("BUS7", edgeOpts{tags: []string{"MES-2"}})}
	}

	tests := []struct {
		eventType EventType
		want      string
	}{
		{
			eventType: Notification,
			want:      "Tag: MES, Processed: 2, Discarded: 1, Unmatched: 1, Expired: 0, EventType: notification, Destinations: BUS1,BUS9",
		},
		{
			eventType: Query,
			want:      "Tag: MES, Processed: 2, Discarded: 1, Unmatched: 1, Expired: 0, EventType: query",
		},
	}

	for _, tt := range tests {
		t.Run(tt.eventType.String(), func(t *testing.T) {
			resetCaches()
			seedSchedule()
			logp.ObserverLogs().TakeAll()

			bt := newTestBeat(t, beatOpts{})
			bt.client = &recordingClient{}

			bt.BuildEvents("MES", edges(), tt.eventType)

			var found bool
			for _, entry := range logp.ObserverLogs().TakeAll() {
				if strings.HasPrefix(entry.Message, "Tag: ") {
					found = true
					if entry.Message != tt.want {
						t.Errorf("summary log:\n got  %q\n want %q", entry.Message, tt.want)
					}
				}
			}

			if !found {
				t.Error("no ProcessResults summary log")
			}
		})
	}
}

func TestEventDestination(t *testing.T) {
	tests := []struct {
		name   string
		fields mapstr.M
		want   string
	}{
		{name: "destination label", fields: mapstr.M{"destinationLabel": "BSITAL", "dstName": "SUB-37107"}, want: "BSITAL"},
		{name: "mapping disabled uses the magnum name", fields: mapstr.M{"dstName": "SUB-37107"}, want: "SUB-37107"},
		{name: "blank label uses the magnum name", fields: mapstr.M{"destinationLabel": "", "dstName": "SUB-37107"}, want: "SUB-37107"},
		{name: "nothing", fields: mapstr.M{}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := eventDestination(&beat.Event{Fields: tt.fields}); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProcessResultsSelector(t *testing.T) {
	tests := []struct {
		selectors []string
		want      []string // event types that are logged, as "logger name: message"
	}{
		{selectors: []string{"ProcessResults.Query"}, want: []string{"ProcessResults.Query: query"}},
		{selectors: []string{"ProcessResults.Notifications"}, want: []string{"ProcessResults.Notifications: notification"}},
		{
			selectors: []string{"ProcessResults.Query", "ProcessResults.Notifications"},
			want:      []string{"ProcessResults.Query: query", "ProcessResults.Notifications: notification"},
		},
		{selectors: []string{"ProcessResults.*"}, want: []string{"ProcessResults.*: query", "ProcessResults.*: notification"}},
		{selectors: []string{"ProcessResults"}, want: []string{"ProcessResults: query", "ProcessResults: notification"}},
		{selectors: []string{"*"}, want: []string{"ProcessResults.Query: query", "ProcessResults.Notifications: notification"}},
		{selectors: []string{"ScheduleCache"}, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.selectors[0], func(t *testing.T) {
			if err := logp.DevelopmentSetup(logp.ToObserverOutput(), logp.WithSelectors(tt.selectors...)); err != nil {
				t.Fatal(err)
			}

			for _, et := range []EventType{Query, Notification} {
				logp.Debug(processResultsSelector(et), "%s", et)
			}

			var got []string
			for _, entry := range logp.ObserverLogs().TakeAll() {
				got = append(got, entry.LoggerName+": "+entry.Message)
			}

			if !slices.Equal(got, tt.want) {
				t.Errorf("selectors %q logged:\n got  %q\n want %q", tt.selectors, got, tt.want)
			}
		})
	}
}
