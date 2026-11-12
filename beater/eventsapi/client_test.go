package eventsapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

const apgEvent = `{"task":"apg","output":"BUS1","input":"SRC1","tags":["MAIN"],"start_date":"2026/08/16 13:00:00","end_date":"2026/08/16 14:00:00"}`

func TestNewValidation(t *testing.T) {
	for _, u := range []string{"", "http://host/api/{output}/events", "host/api", "://bad"} {
		if _, err := New(Config{Url: u}); err == nil {
			t.Errorf("New(%q) expected an error", u)
		}
	}

	c, err := New(Config{Url: "http://host/api"})
	if err != nil {
		t.Fatal(err)
	}

	if c.timeout != defaultTimeout || c.bulkTimeout != defaultBulkTimeout || c.startParam != "start" ||
		c.endParam != "end" || c.outputParam != "output" || c.taskParam != "task" || c.task != "" {
		t.Errorf("defaults = %+v", c)
	}
}

func TestRequestURL(t *testing.T) {
	c, err := New(Config{Url: "http://host/api/v1/scheduler/events?site=a", Task: "apg"})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		params url.Values
		want   string
	}{
		{
			name:   "now",
			params: url.Values{"start": {"now"}, "end": {"now"}},
			want:   "http://host/api/v1/scheduler/events?end=now&site=a&start=now&task=apg",
		},
		{
			name:   "lookahead plus is encoded",
			params: url.Values{"start": {"now+25m"}, "end": {"now+25m"}},
			want:   "http://host/api/v1/scheduler/events?end=now%2B25m&site=a&start=now%2B25m&task=apg",
		},
		{
			name:   "output",
			params: url.Values{"output": {"BUS 1"}},
			want:   "http://host/api/v1/scheduler/events?output=BUS+1&site=a&task=apg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.RequestURL(tt.params); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}

	t.Run("no task filter", func(t *testing.T) {
		c, _ := New(Config{Url: "http://host/events"})
		if got := c.RequestURL(url.Values{"output": {"BUS1"}}); got != "http://host/events?output=BUS1" {
			t.Errorf("got %s", got)
		}
	})
}

// stub records the query of each request and replies with body
func stub(t *testing.T, status int, body string) (*httptest.Server, func() []url.Values) {
	t.Helper()

	var (
		mu      sync.Mutex
		queries []url.Values
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "yes" || r.Header.Get("Accept") != "application/json" {
			t.Error("headers were not sent")
		}

		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()

		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return queries
	}
}

func newClient(t *testing.T, srvURL string, cfg Config) *Client {
	t.Helper()

	cfg.Url = srvURL
	cfg.Headers = map[string]string{"X-Test": "yes"}

	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func TestEventsAt(t *testing.T) {
	srv, queries := stub(t, http.StatusOK, `[`+apgEvent+`,{"task":"route"}]`)

	c := newClient(t, srv.URL, Config{StartParam: "start_date", EndParam: "end_date", TaskParam: "event_task", Task: "apg"})

	events, stats, err := c.EventsAt(context.Background(), "now+25m")
	if err != nil {
		t.Fatal(err)
	}

	if len(events) != 1 || events[0].Output != "BUS1" || stats.NonApg != 1 {
		t.Errorf("events = %+v stats = %+v", events, stats)
	}

	q := queries()
	if len(q) != 1 {
		t.Fatalf("requests = %d, want 1", len(q))
	}

	want := url.Values{"start_date": {"now+25m"}, "end_date": {"now+25m"}, "event_task": {"apg"}}
	if q[0].Encode() != want.Encode() {
		t.Errorf("query = %v, want %v", q[0], want)
	}
}

func TestRefreshOutput(t *testing.T) {
	srv, queries := stub(t, http.StatusOK, apgEvent)

	c := newClient(t, srv.URL, Config{Task: "apg"})

	events, _, err := c.RefreshOutput(context.Background(), "BUS1")
	if err != nil {
		t.Fatal(err)
	}

	if len(events) != 1 || events[0].Input != "SRC1" {
		t.Errorf("events = %+v", events)
	}

	want := url.Values{"output": {"BUS1"}, "task": {"apg"}}
	if q := queries(); len(q) != 1 || q[0].Encode() != want.Encode() {
		t.Errorf("query = %v, want %v", q, want)
	}
}

func TestEventsErrors(t *testing.T) {
	t.Run("non-2xx", func(t *testing.T) {
		srv, _ := stub(t, http.StatusInternalServerError, apgEvent)

		if events, _, err := newClient(t, srv.URL, Config{}).EventsAt(context.Background(), "now"); err == nil || events != nil {
			t.Errorf("events = %v err = %v, want an error", events, err)
		}
	})

	t.Run("malformed response", func(t *testing.T) {
		srv, _ := stub(t, http.StatusOK, `[{"task":"apg"`)

		if _, _, err := newClient(t, srv.URL, Config{}).RefreshOutput(context.Background(), "BUS1"); err == nil {
			t.Error("expected a parse error")
		}
	})

	slow := func(t *testing.T) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(time.Second):
			case <-r.Context().Done():
			}
			w.Write([]byte("[]"))
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("refresh uses the timeout", func(t *testing.T) {
		c, _ := New(Config{Url: slow(t).URL, Timeout: 50 * time.Millisecond, BulkTimeout: 5 * time.Second})

		if _, _, err := c.RefreshOutput(context.Background(), "BUS1"); err == nil {
			t.Error("expected a timeout error")
		}
	})

	t.Run("events at uses the bulk timeout", func(t *testing.T) {
		c, _ := New(Config{Url: slow(t).URL, Timeout: 50 * time.Millisecond, BulkTimeout: 5 * time.Second})

		if _, _, err := c.EventsAt(context.Background(), "now"); err != nil {
			t.Errorf("err = %v, the bulk request should outlast the refresh timeout", err)
		}

		c, _ = New(Config{Url: slow(t).URL, Timeout: 5 * time.Second, BulkTimeout: 50 * time.Millisecond})

		if _, _, err := c.EventsAt(context.Background(), "now"); err == nil {
			t.Error("expected a bulk timeout error")
		}
	})
}
