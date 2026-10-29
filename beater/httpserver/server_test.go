package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a callback that records each payload it receives
type recorder struct {
	mu       sync.Mutex
	payloads []string
	err      error
}

func (r *recorder) callback(_ context.Context, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.payloads = append(r.payloads, string(payload))
	return r.err
}

func (r *recorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.payloads...)
}

// freePort returns a port that is free at the time of the call
func freePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	return ln.Addr().(*net.TCPAddr).Port
}

func decodeBody(t *testing.T, body io.Reader) map[string]string {
	t.Helper()

	var m map[string]string
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		t.Fatalf("response body is not a json object: %v", err)
	}

	return m
}

// errReader fails every read
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestNewPort(t *testing.T) {
	tests := []struct {
		port int
		want string
	}{
		{port: 8082, want: ":8082"},
		{port: 1, want: ":1"},
		{port: 65535, want: ":65535"},
		{port: 0, want: ":8080"},
		{port: -1, want: ":8080"},
		{port: 65536, want: ":8080"},
	}

	for _, tt := range tests {
		if s := New(tt.port, nil, nil); s.addr != tt.want {
			t.Errorf("New(%d) addr = %q, want %q", tt.port, s.addr, tt.want)
		}
	}
}

func TestSchedulerEventRouting(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantCalls  int
	}{
		{name: "post scheduler events", method: http.MethodPost, path: schedulerEventsPath, body: `[{"task":"apg"}]`, wantStatus: http.StatusAccepted, wantCalls: 1},
		{name: "get is not allowed", method: http.MethodGet, path: schedulerEventsPath, wantStatus: http.StatusMethodNotAllowed},
		{name: "put is not allowed", method: http.MethodPut, path: schedulerEventsPath, body: "{}", wantStatus: http.StatusMethodNotAllowed},
		{name: "unknown path", method: http.MethodPost, path: "/api/v1/other", body: "{}", wantStatus: http.StatusNotFound},
		{name: "trailing slash is a different path", method: http.MethodPost, path: schedulerEventsPath + "/", body: "{}", wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			s := New(8082, nil, rec.callback)

			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body)))

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}

			if got := len(rec.calls()); got != tt.wantCalls {
				t.Errorf("callback calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestHandleSchedulerEvent(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		body        io.Reader
		callbackErr error
		nilCallback bool
		wantStatus  int
		wantKey     string // key in the json response body
		wantValue   string // "" = only check the key exists
		wantPayload string // payload passed to the callback, "" = callback not called
	}{
		{
			name:        "accepted",
			method:      http.MethodPost,
			body:        strings.NewReader(`{"task":"apg","output":"PMQE04"}`),
			wantStatus:  http.StatusAccepted,
			wantKey:     "status",
			wantValue:   "accepted",
			wantPayload: `{"task":"apg","output":"PMQE04"}`,
		},
		{
			name:        "payload is passed through unmodified",
			method:      http.MethodPost,
			body:        strings.NewReader("  \n[1, 2]\n"),
			wantStatus:  http.StatusAccepted,
			wantKey:     "status",
			wantValue:   "accepted",
			wantPayload: "  \n[1, 2]\n",
		},
		{
			// a callback error rejects the payload
			name:        "callback error rejects the payload",
			method:      http.MethodPost,
			body:        strings.NewReader(`{"task":`),
			callbackErr: errors.New("malformed JSON object payload"),
			wantStatus:  http.StatusBadRequest,
			wantKey:     "error",
			wantValue:   "invalid payload: malformed JSON object payload",
			wantPayload: `{"task":`,
		},
		{
			name:       "non-post method",
			method:     http.MethodGet,
			body:       http.NoBody,
			wantStatus: http.StatusMethodNotAllowed,
			wantKey:    "error",
			wantValue:  "method not allowed",
		},
		{
			name:       "empty payload",
			method:     http.MethodPost,
			body:       http.NoBody,
			wantStatus: http.StatusBadRequest,
			wantKey:    "error",
			wantValue:  "request payload is empty",
		},
		{
			name:       "body read error",
			method:     http.MethodPost,
			body:       errReader{},
			wantStatus: http.StatusBadRequest,
			wantKey:    "error",
		},
		{
			name:        "callback not configured",
			method:      http.MethodPost,
			body:        strings.NewReader("{}"),
			nilCallback: true,
			wantStatus:  http.StatusInternalServerError,
			wantKey:     "error",
			wantValue:   "callback not configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{err: tt.callbackErr}

			cb := rec.callback
			if tt.nilCallback {
				cb = nil
			}

			s := New(8082, nil, cb)

			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(tt.method, schedulerEventsPath, tt.body))

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}

			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type = %q, want application/json", ct)
			}

			body := decodeBody(t, w.Body)
			if v, ok := body[tt.wantKey]; !ok || (tt.wantValue != "" && v != tt.wantValue) {
				t.Errorf("body = %v, want %s=%q", body, tt.wantKey, tt.wantValue)
			}

			calls := rec.calls()
			switch {
			case tt.wantPayload == "" && len(calls) != 0:
				t.Errorf("callback should not be called, got %q", calls)
			case tt.wantPayload != "" && (len(calls) != 1 || calls[0] != tt.wantPayload):
				t.Errorf("callback payloads = %q, want [%q]", calls, tt.wantPayload)
			}
		})
	}
}

func TestHandleSchedulerEventMissingBody(t *testing.T) {
	rec := &recorder{}
	s := New(8082, nil, rec.callback)

	req := httptest.NewRequest(http.MethodPost, schedulerEventsPath, nil)
	req.Body = nil

	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	if body := decodeBody(t, w.Body); body["error"] != "request body is required" {
		t.Errorf("body = %v", body)
	}

	if len(rec.calls()) != 0 {
		t.Error("callback should not be called")
	}
}

func TestHandleSchedulerEventPassesRequestContext(t *testing.T) {
	type ctxKey struct{}

	var got any
	s := New(8082, nil, func(ctx context.Context, _ []byte) error {
		got = ctx.Value(ctxKey{})
		return nil
	})

	req := httptest.NewRequest(http.MethodPost, schedulerEventsPath, strings.NewReader("{}"))
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, "request"))

	s.ServeHTTP(httptest.NewRecorder(), req)

	if got != "request" {
		t.Errorf("callback context value = %v, want the request context", got)
	}
}

func TestWriteJSON(t *testing.T) {
	t.Run("status body and headers", func(t *testing.T) {
		w := httptest.NewRecorder()

		extra := http.Header{"X-Request-Id": []string{"abc"}, "Content-Type": []string{"text/plain"}}

		if err := WriteJSON(w, http.StatusCreated, map[string]string{"status": "ok"}, extra); err != nil {
			t.Fatal(err)
		}

		if w.Code != http.StatusCreated {
			t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
		}

		if got := w.Header().Get("X-Request-Id"); got != "abc" {
			t.Errorf("X-Request-Id = %q, want abc", got)
		}

		// content type is always json, even if an extra header tries to override it
		if got := w.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("content-type = %q, want application/json", got)
		}

		if got := w.Body.String(); got != `{"status":"ok"}` {
			t.Errorf("body = %s", got)
		}
	})

	t.Run("marshal error writes nothing", func(t *testing.T) {
		w := httptest.NewRecorder()

		if err := WriteJSON(w, http.StatusOK, make(chan int)); err == nil {
			t.Fatal("expected a marshal error")
		}

		if w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" {
			t.Errorf("nothing should be written, got body %q headers %v", w.Body.String(), w.Header())
		}
	})
}

func TestStartServesAndShutsDown(t *testing.T) {
	port := freePort(t)
	done := make(chan struct{})
	rec := &recorder{}

	s := New(port, done, rec.callback)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-s.listening:
	default:
		t.Fatal("listening channel should be closed after Start")
	}

	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, schedulerEventsPath)
	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Post(url, "application/json", strings.NewReader(`[{"task":"apg"}]`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	if calls := rec.calls(); len(calls) != 1 || calls[0] != `[{"task":"apg"}]` {
		t.Errorf("callback payloads = %q", calls)
	}

	// closing done shuts the server down and frees the port
	close(done)

	deadline := time.Now().Add(5 * time.Second)
	for {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			ln.Close()
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("port %d still in use after shutdown: %v", port, err)
		}

		time.Sleep(20 * time.Millisecond)
	}

	if _, err := client.Post(url, "application/json", strings.NewReader("{}")); err == nil {
		t.Error("request should fail after shutdown")
	}
}

func TestStartPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	s := New(ln.Addr().(*net.TCPAddr).Port, nil, (&recorder{}).callback)

	if err := s.Start(); err == nil {
		t.Fatal("expected an error when the port is in use")
	}

	select {
	case <-s.listening:
		t.Error("listening channel should stay open when Start fails")
	default:
	}
}

func TestStartWithoutDoneChannel(t *testing.T) {
	port := freePort(t)

	// a nil done channel means the server is never shut down by the routine
	s := New(port, nil, (&recorder{}).callback)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.server.Close() })

	resp, err := (&http.Client{Timeout: 2 * time.Second}).Post(
		fmt.Sprintf("http://127.0.0.1:%d%s", port, schedulerEventsPath), "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
}
