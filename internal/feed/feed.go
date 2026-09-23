// SPDX-License-Identifier: GPL-3.0-or-later

// Package feed keeps a bounded history of completed turns and serves it,
// live, as a server-sent event stream and a mobile-friendly web page.
package feed

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/joncbenderkh/cc-proxy/internal/transcript"
	"github.com/joncbenderkh/cc-proxy/internal/usage"
)

// Turn is one Messages API exchange as the feed shows it.
type Turn struct {
	Seq        int64              `json:"seq"`
	Time       time.Time          `json:"time"`
	SessionID  string             `json:"session_id,omitempty"`
	Status     int                `json:"status"`
	DurationMs int64              `json:"duration_ms"`
	Message    *usage.Message     `json:"message,omitempty"`
	Prompt     []transcript.Block `json:"prompt,omitempty"`
	Reply      []transcript.Block `json:"reply,omitempty"`
}

// subscriberBuffer is how many turns a slow client may fall behind before
// it is disconnected; it then reconnects and resumes from Last-Event-ID.
const subscriberBuffer = 64

// Hub fans turns out to subscribers and remembers the most recent ones.
type Hub struct {
	mu          sync.Mutex
	history     []Turn
	capacity    int
	nextSeq     int64
	subscribers map[chan Turn]struct{}
	closed      bool
}

// NewHub returns a hub that remembers the last capacity turns.
func NewHub(capacity int) *Hub {
	return &Hub{capacity: capacity, nextSeq: 1, subscribers: map[chan Turn]struct{}{}}
}

// Publish numbers turn, stores it and delivers it to every subscriber
// without blocking.
func (h *Hub) Publish(turn Turn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	turn.Seq = h.nextSeq
	h.nextSeq++
	h.history = append(h.history, turn)
	if len(h.history) > h.capacity {
		h.history = h.history[len(h.history)-h.capacity:]
	}
	for ch := range h.subscribers {
		select {
		case ch <- turn:
		default:
			delete(h.subscribers, ch)
			close(ch)
		}
	}
}

// Close disconnects every subscriber and ignores later turns.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.subscribers {
		delete(h.subscribers, ch)
		close(ch)
	}
}

// subscribe returns the remembered turns after seq and a channel of the
// turns published from now on. The channel is closed when the subscriber
// falls behind or the hub closes; cancel releases it early.
func (h *Hub) subscribe(after int64) (backlog []Turn, turns <-chan Turn, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, turn := range h.history {
		if turn.Seq > after {
			backlog = append(backlog, turn)
		}
	}
	ch := make(chan Turn, subscriberBuffer)
	if h.closed {
		close(ch)
		return backlog, ch, func() {}
	}
	h.subscribers[ch] = struct{}{}
	return backlog, ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subscribers[ch]; ok {
			delete(h.subscribers, ch)
			close(ch)
		}
	}
}

//go:embed index.html
var indexHTML []byte

const heartbeatInterval = 25 * time.Second

// Handler serves the web page at / and the turn stream at /events.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /events", h.serveEvents)
	return mux
}

func (h *Hub) serveEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	backlog, turns, cancel := h.subscribe(after)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher := http.NewResponseController(w)
	for _, turn := range backlog {
		if writeTurn(w, turn) != nil {
			return
		}
	}
	if flusher.Flush() != nil {
		return
	}
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case turn, ok := <-turns:
			if !ok || writeTurn(w, turn) != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
		}
		if flusher.Flush() != nil {
			return
		}
	}
}

func writeTurn(w http.ResponseWriter, turn Turn) error {
	data, err := json.Marshal(turn)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: turn\ndata: %s\n\n", turn.Seq, data)
	return err
}
