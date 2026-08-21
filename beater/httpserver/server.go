package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
)

// CallbackFunc is a generic callback executed after a valid payload is decoded.
// It should contain cache update logic or any other business processing.
type CallbackFunc[T any] func(ctx context.Context, payload T) error

const schedulerEventsPath = "/api/v1/scheduler/events"

// Server implements a minimal HTTP server for scheduler event updates.
type Server[T any] struct {
	addr      string
	done      <-chan struct{}
	callback  CallbackFunc[T]
	mux       *http.ServeMux
	server    *http.Server
	listening chan struct{}
}

// New creates an HTTP server configured to listen on the given port, watch the shutdown
// channel, and route POST requests to a generic callback.
func New[T any](port int, done <-chan struct{}, callback CallbackFunc[T]) *Server[T] {
	if port <= 0 || port > 65535 {
		port = 8080
	}

	s := &Server[T]{
		addr:      fmt.Sprintf(":%d", port),
		done:      done,
		callback:  callback,
		mux:       http.NewServeMux(),
		listening: make(chan struct{}),
	}

	s.mux.HandleFunc("POST "+schedulerEventsPath, s.handleSchedulerEvent)
	return s
}

// Start starts the HTTP server in a background goroutine.
func (s *Server[T]) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		logp.Err("HTTP hook server failed to listen on %s: %v", s.addr, err)
		return err
	}

	close(s.listening)
	logp.Info("HTTP hook server started on %s", s.addr)

	s.server = &http.Server{
		Addr:              s.addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	// Shutdown goroutine to gracefully stop the server when done channel is closed.
	go func() {
		if s.done == nil {
			return
		}
		<-s.done
		logp.Warn("HTTP hook server shutdown requested for %s", s.addr)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(ctx); err != nil {
			logp.Err("HTTP hook server shutdown failed for %s: %v", s.addr, err)
		}
	}()

	// Serve in a background goroutine.
	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			logp.Err("HTTP hook server serve loop failed for %s: %v", s.addr, err)
			return
		}
	}()

	return nil
}

// ServeHTTP exposes the server as an http.Handler for testing and embedding.
func (s *Server[T]) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handleSchedulerEvent(w, r)
}

func (s *Server[T]) handleSchedulerEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		logp.Warn("scheduler event endpoint rejected non-POST method: %s", r.Method)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = fmt.Fprintf(w, `{"error":"method not allowed"}`)
		return
	}

	if r.Body == nil {
		logp.Err("scheduler event request body is missing")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":"request body is required"}`)
		return
	}
	defer r.Body.Close()

	var payload T
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		logp.Err("scheduler event payload decode failed: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":"invalid request payload: %v"}`, err)
		return
	}

	if s.callback == nil {
		logp.Err("scheduler event callback is not configured")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"error":"callback not configured"}`)
		return
	}

	go func() {
		if err := s.callback(r.Context(), payload); err != nil {
			logp.Err("scheduler event callback failed: %v", err)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintf(w, `{"status":"accepted"}`)
}
