// SPDX-License-Identifier: GPL-3.0-or-later

// Package feed keeps a bounded history of completed turns, plus named
// live state such as pending approvals, and serves both as a server-sent
// event stream and a mobile-friendly web page.
package feed

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
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
	Cwd        string             `json:"cwd,omitempty"`
	Title      string             `json:"title,omitempty"`
	Status     int                `json:"status"`
	DurationMs int64              `json:"duration_ms"`
	Message    *usage.Message     `json:"message,omitempty"`
	Prompt     []transcript.Block `json:"prompt,omitempty"`
	Reply      []transcript.Block `json:"reply,omitempty"`
}

// subscriberBuffer is how many events a slow client may fall behind before
// it is disconnected; it then reconnects and resumes from Last-Event-ID.
const subscriberBuffer = 64

// event is one server-sent event. Turns carry their sequence number as the
// event id; state events carry none, so they never move Last-Event-ID.
type event struct {
	id   int64
	name string
	data []byte
}

// Hub fans turns and state changes out to subscribers. It remembers the
// most recent turns and the latest value of each state.
type Hub struct {
	mu          sync.Mutex
	history     []event
	capacity    int
	nextSeq     int64
	states      map[string][]byte
	subscribers map[chan event]struct{}
	closed      bool
}

// NewHub returns a hub that remembers the last capacity turns.
func NewHub(capacity int) *Hub {
	return &Hub{capacity: capacity, nextSeq: 1, states: map[string][]byte{}, subscribers: map[chan event]struct{}{}}
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
	data, err := json.Marshal(turn)
	if err != nil {
		return
	}
	ev := event{id: turn.Seq, name: "turn", data: data}
	h.history = append(h.history, ev)
	if len(h.history) > h.capacity {
		h.history = h.history[len(h.history)-h.capacity:]
	}
	h.broadcast(ev)
}

// SetState replaces the value of the named state, sends it to every
// subscriber, and sends it to later subscribers when they connect.
func (h *Hub) SetState(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.states[name] = data
	h.broadcast(event{name: name, data: data})
	return nil
}

func (h *Hub) broadcast(ev event) {
	for ch := range h.subscribers {
		select {
		case ch <- ev:
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

// subscribe returns the remembered turns after seq followed by every
// state, and a channel of the events published from now on. The channel is
// closed when the subscriber falls behind or the hub closes; cancel
// releases it early.
func (h *Hub) subscribe(after int64) (backlog []event, events <-chan event, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ev := range h.history {
		if ev.id > after {
			backlog = append(backlog, ev)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(h.states)) {
		backlog = append(backlog, event{name: name, data: h.states[name]})
	}
	ch := make(chan event, subscriberBuffer)
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
	backlog, events, cancel := h.subscribe(after)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher := http.NewResponseController(w)
	for _, ev := range backlog {
		if writeEvent(w, ev) != nil {
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
		case ev, ok := <-events:
			if !ok || writeEvent(w, ev) != nil {
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

func writeEvent(w http.ResponseWriter, ev event) error {
	if ev.id != 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", ev.id); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data)
	return err
}
