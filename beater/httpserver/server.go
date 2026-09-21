package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
)

// CallbackFunc is executed with the raw request payload bytes.
// It should contain parsing and business processing logic.
type CallbackFunc func(ctx context.Context, payload []byte) error

const schedulerEventsPath = "/api/v1/scheduler/events"

// Server implements a minimal HTTP server for scheduler event updates.
type Server struct {
	addr      string
	done      <-chan struct{}
	callback  CallbackFunc
	mux       *http.ServeMux
	server    *http.Server
	listening chan struct{}
}

// New creates an HTTP server configured to listen on the given port, watch the shutdown
// channel, and route POST requests to a generic callback.
func New(port int, done <-chan struct{}, callback CallbackFunc) *Server {
	if port <= 0 || port > 65535 {
		port = 8080
	}

	s := &Server{
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
func (s *Server) Start() error {
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
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handleSchedulerEvent(w, r)
}

func (s *Server) handleSchedulerEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		logp.Warn("scheduler event endpoint rejected non-POST method: %s", r.Method)
		WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	if r.Body == nil {
		logp.Err("scheduler event request body is missing")
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "request body is required"})
		return
	}
	defer r.Body.Close()

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		logp.Err("scheduler event payload read failed: %v", err)
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid request payload: %v", err)})
		return
	}

	if len(payload) == 0 {
		logp.Err("scheduler event payload is empty")
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "request payload is empty"})
		return
	}

	if s.callback == nil {
		logp.Err("scheduler event callback is not configured")
		WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": "callback not configured"})
		return
	}

	if err := s.callback(r.Context(), payload); err != nil {
		logp.Err("scheduler event callback failed: %v", err)
	}

	WriteJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func WriteJSON(w http.ResponseWriter, status int, data any, headers ...http.Header) error {
	out, err := json.Marshal(data)
	if err != nil {
		return err
	}

	// add additional header if provided (variadic parameter)
	if len(headers) > 0 {
		maps.Copy(w.Header(), headers[0])
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_, err = w.Write(out)
	if err != nil {
		return err
	}

	return nil
}
