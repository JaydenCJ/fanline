// Package server exposes the hub over HTTP: an SSE subscribe endpoint, a
// publish endpoint, stats, and health — with token auth on everything but
// health.
//
// Proxy friendliness is a design goal: responses disable buffering
// (X-Accel-Buffering: no), send periodic comment keepalives so idle
// connections are not reaped, and advertise a client retry interval. The
// token may arrive as a Bearer header or, because the browser EventSource
// API cannot set headers, as a `?token=` query parameter.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JaydenCJ/fanline/internal/hub"
	"github.com/JaydenCJ/fanline/internal/sse"
	"github.com/JaydenCJ/fanline/internal/token"
	"github.com/JaydenCJ/fanline/internal/version"
)

// Config tunes the HTTP layer. Zero values select documented defaults.
type Config struct {
	Keys       token.Keyring // verification keys; ignored in Dev mode
	Dev        bool          // no auth — loopback development only
	CORSOrigin string        // Access-Control-Allow-Origin value; "" disables CORS
	KeepAlive  time.Duration // comment keepalive interval (default 25s)
	RetryMS    int           // client reconnect hint (default 3000)
	MaxBody    int64         // publish body cap in bytes (default 256 KiB)
	Now        func() time.Time
}

func (c Config) withDefaults() Config {
	if c.KeepAlive <= 0 {
		c.KeepAlive = 25 * time.Second
	}
	if c.RetryMS <= 0 {
		c.RetryMS = 3000
	}
	if c.MaxBody <= 0 {
		c.MaxBody = 256 << 10
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

type server struct {
	hub *hub.Hub
	cfg Config
}

// New returns the fanline HTTP handler for h.
func New(h *hub.Hub, cfg Config) http.Handler {
	s := &server{hub: h, cfg: cfg.withDefaults()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/stats", s.handleStats)
	mux.HandleFunc("POST /v1/publish/{channel}", s.handlePublish)
	mux.HandleFunc("GET /v1/sse/{channel}", s.handleSubscribe)
	return s.withCORS(mux)
}

// --- middleware -----------------------------------------------------------

// withCORS adds the configured Access-Control headers and answers
// preflight OPTIONS requests before they reach the mux.
func (s *server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.CORSOrigin != "" {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", s.cfg.CORSOrigin)
			if s.cfg.CORSOrigin != "*" {
				h.Set("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID, X-Fanline-Event")
				h.Set("Access-Control-Max-Age", "86400")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authorize verifies the request's token and checks capability cap on
// channel ch. In Dev mode every request is allowed.
func (s *server) authorize(w http.ResponseWriter, r *http.Request, cap, ch string) bool {
	if s.cfg.Dev {
		return true
	}
	tok := bearerToken(r)
	if tok == "" {
		httpError(w, http.StatusUnauthorized, "missing token (Authorization: Bearer … or ?token=…)")
		return false
	}
	claims, err := token.Verify(tok, s.cfg.Keys, s.cfg.Now())
	if err != nil {
		status := http.StatusUnauthorized
		msg := "invalid token"
		switch {
		case errors.Is(err, token.ErrExpired):
			msg = "token expired"
		case errors.Is(err, token.ErrNotYetValid):
			msg = "token not valid yet"
		}
		httpError(w, status, msg)
		return false
	}
	if !claims.Allows(cap, ch) {
		httpError(w, http.StatusForbidden,
			fmt.Sprintf("token does not grant %q on channel %q", cap, ch))
		return false
	}
	return true
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if t, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(t)
		}
		return ""
	}
	return r.URL.Query().Get("token")
}

// --- handlers -------------------------------------------------------------

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\"ok\":true,\"version\":%q}\n", version.Version)
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(w, r, token.CapStats, "") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  version.Version,
		"channels": s.hub.Stats(),
	})
}

// publishResponse is the JSON body returned by POST /v1/publish/{channel}.
type publishResponse struct {
	ID          string `json:"id"`
	Seq         uint64 `json:"seq"`
	Channel     string `json:"channel"`
	Subscribers int    `json:"subscribers"`
}

func (s *server) handlePublish(w http.ResponseWriter, r *http.Request) {
	ch := r.PathValue("channel")
	if !s.authorize(w, r, token.CapPublish, ch) {
		return
	}
	eventName := r.Header.Get("X-Fanline-Event")
	if eventName == "" {
		eventName = r.URL.Query().Get("event")
	}
	if err := validateEventName(eventName); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBody))
	if err != nil {
		httpError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("body exceeds %d bytes", s.cfg.MaxBody))
		return
	}
	e, delivered, err := s.hub.Publish(ch, eventName, string(body))
	if err != nil {
		if errors.Is(err, hub.ErrTooManyChannels) {
			httpError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, publishResponse{
		ID: e.ID, Seq: e.Seq, Channel: ch, Subscribers: delivered,
	})
}

// readyPayload is the JSON data of the `fanline.ready` event sent first on
// every SSE connection, so clients know what replay (if any) follows.
type readyPayload struct {
	Channel  string `json:"channel"`
	Epoch    string `json:"epoch"`
	Replayed int    `json:"replayed"`
	Gap      bool   `json:"gap"`
}

func (s *server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	ch := r.PathValue("channel")
	if !s.authorize(w, r, token.CapSubscribe, ch) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported by this connection")
		return
	}
	lastID := r.Header.Get("Last-Event-ID")
	if lastID == "" {
		lastID = r.URL.Query().Get("last_event_id")
	}
	replayN := 0
	if v := r.URL.Query().Get("replay"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			httpError(w, http.StatusBadRequest, "replay must be a non-negative integer")
			return
		}
		replayN = n
	}

	sub, err := s.hub.Subscribe(ch, lastID, replayN)
	if err != nil {
		if errors.Is(err, hub.ErrTooManyChannels) {
			httpError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // tell nginx-style proxies not to buffer
	w.WriteHeader(http.StatusOK)

	ready, _ := json.Marshal(readyPayload{
		Channel: ch, Epoch: sub.Epoch, Replayed: len(sub.Replay), Gap: sub.Gap,
	})
	_ = sse.WriteRetry(w, s.cfg.RetryMS)
	_ = sse.WriteEvent(w, sse.Event{Name: "fanline.ready", Data: string(ready)})
	for _, e := range sub.Replay {
		_ = sse.WriteEvent(w, sse.Event{ID: e.ID, Name: e.Name, Data: e.Data})
	}
	flusher.Flush()

	keepalive := time.NewTicker(s.cfg.KeepAlive)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, open := <-sub.Events():
			if !open {
				// Hub dropped us (slow consumer). End the response; the
				// client reconnects and resumes via Last-Event-ID.
				return
			}
			if err := sse.WriteEvent(w, sse.Event{ID: e.ID, Name: e.Name, Data: e.Data}); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if err := sse.WriteComment(w, "keepalive"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// --- helpers --------------------------------------------------------------

// validateEventName keeps SSE event names single-line and short. Names
// beginning "fanline." are reserved for protocol events like ready.
func validateEventName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > 64 {
		return errors.New("event name exceeds 64 characters")
	}
	if strings.HasPrefix(name, "fanline.") {
		return errors.New("event names starting with \"fanline.\" are reserved")
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.'
		if !ok {
			return fmt.Errorf("event name contains %q (allowed: a-z A-Z 0-9 _ - .)", r)
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
