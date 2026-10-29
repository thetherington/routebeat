package eventsapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func noopCallback(context.Context, []byte) error { return nil }

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}, noopCallback); err == nil {
		t.Error("expected an error for an empty url")
	}

	if _, err := New(Config{Url: "http://host"}, nil); err == nil {
		t.Error("expected an error for a nil callback")
	}

	c, err := New(Config{Url: "http://host"}, noopCallback)
	if err != nil || c.timeout != defaultTimeout {
		t.Errorf("got %v %v, want the default timeout", c, err)
	}
}

func TestRequestURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{url: "http://host/api/{output}/events", want: "http://host/api/BUS%201/events"},
		{url: "http://host/api/events", want: "http://host/api/events?output=BUS+1"},
		{url: "http://host/api/events?site=a", want: "http://host/api/events?output=BUS+1&site=a"},
	}

	for _, tt := range tests {
		c := &Client{url: tt.url}

		got, err := c.requestURL("BUS 1")
		if err != nil || got != tt.want {
			t.Errorf("requestURL(%q) = %q %v, want %q", tt.url, got, err, tt.want)
		}
	}
}

func TestRefresh(t *testing.T) {
	t.Run("runs the callback with the payload", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Test") != "yes" {
				t.Error("configured header was not sent")
			}
			w.Write([]byte(`[{"output":"` + r.URL.Query().Get("output") + `"}]`))
		}))
		defer srv.Close()

		var got string
		callback := func(_ context.Context, payload []byte) error {
			got = string(payload)
			return nil
		}

		c, err := New(Config{Url: srv.URL, Timeout: time.Second, Headers: map[string]string{"X-Test": "yes"}}, callback)
		if err != nil {
			t.Fatal(err)
		}

		if err := c.Refresh(context.Background(), "BUS1"); err != nil {
			t.Fatal(err)
		}

		if got != `[{"output":"BUS1"}]` {
			t.Errorf("callback payload = %s", got)
		}
	})

	t.Run("callback error is returned", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{}`))
		}))
		defer srv.Close()

		errCallback := errors.New("callback failed")
		c, _ := New(Config{Url: srv.URL}, func(context.Context, []byte) error { return errCallback })

		if err := c.Refresh(context.Background(), "BUS1"); !errors.Is(err, errCallback) {
			t.Errorf("err = %v, want the callback error", err)
		}
	})

	t.Run("non-2xx returns an error without running the callback", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		called := false
		c, _ := New(Config{Url: srv.URL}, func(context.Context, []byte) error { called = true; return nil })

		if err := c.Refresh(context.Background(), "BUS1"); err == nil || called {
			t.Errorf("err = %v, called = %t", err, called)
		}
	})

	t.Run("timeout returns an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(time.Second):
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()

		c, _ := New(Config{Url: srv.URL, Timeout: 50 * time.Millisecond}, noopCallback)

		if err := c.Refresh(context.Background(), "BUS1"); err == nil {
			t.Error("expected a timeout error")
		}
	})
}
