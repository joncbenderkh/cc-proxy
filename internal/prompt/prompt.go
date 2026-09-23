// SPDX-License-Identifier: GPL-3.0-or-later

// Package prompt lets a remote viewer send the next prompt to an idle
// Claude Code session. A background Stop hook waits here once Claude has
// finished responding; a prompt from a viewer ends the wait, and the hook
// hands it to Claude, which wakes up and acts on it.
package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Idle is a session waiting for its next prompt, as viewers see it.
type Idle struct {
	SessionID string    `json:"session_id"`
	Time      time.Time `json:"time"`
	Cwd       string    `json:"cwd,omitempty"`
}

// Outcome is how a wait ended.
type Outcome string

const (
	Prompted   Outcome = "prompted"
	Superseded Outcome = "superseded"
	Canceled   Outcome = "canceled"
)

// ErrNotIdle is returned for a prompt to a session that is not waiting.
var ErrNotIdle = errors.New("session not waiting for a prompt")

// MaxPrompt is the longest prompt a viewer can send, in bytes.
const MaxPrompt = 64 << 10

// Inbox holds the sessions waiting for a prompt.
type Inbox struct {
	publish func([]Idle)
	logger  *slog.Logger

	mu      sync.Mutex
	waiting map[string]*waiter
}

type waiter struct {
	idle       Idle
	prompt     chan string
	superseded chan struct{}
}

// NewInbox returns an inbox that reports every change to the idle list
// through publish.
func NewInbox(publish func([]Idle), logger *slog.Logger) *Inbox {
	in := &Inbox{publish: publish, logger: logger, waiting: map[string]*waiter{}}
	in.publishLocked()
	return in
}

// Wait holds idle until a viewer sends a prompt, the session goes idle
// again with a newer wait, or ctx ends.
func (in *Inbox) Wait(ctx context.Context, idle Idle) (string, Outcome) {
	w := &waiter{idle: idle, prompt: make(chan string, 1), superseded: make(chan struct{})}
	in.mu.Lock()
	if previous := in.waiting[idle.SessionID]; previous != nil {
		close(previous.superseded)
	}
	in.waiting[idle.SessionID] = w
	in.publishLocked()
	in.mu.Unlock()
	defer in.remove(w)

	select {
	case text := <-w.prompt:
		return text, Prompted
	case <-w.superseded:
		return "", Superseded
	case <-ctx.Done():
		return "", Canceled
	}
}

// Send delivers text to the waiting session with the given id.
func (in *Inbox) Send(sessionID, text string) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	w := in.waiting[sessionID]
	if w == nil {
		return ErrNotIdle
	}
	w.prompt <- text
	delete(in.waiting, sessionID)
	in.publishLocked()
	return nil
}

func (in *Inbox) remove(w *waiter) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.waiting[w.idle.SessionID] == w {
		delete(in.waiting, w.idle.SessionID)
		in.publishLocked()
	}
}

func (in *Inbox) publishLocked() {
	idle := make([]Idle, 0, len(in.waiting))
	for _, w := range in.waiting {
		idle = append(idle, w.idle)
	}
	slices.SortFunc(idle, func(a, b Idle) int { return a.Time.Compare(b.Time) })
	in.publish(idle)
}

// Register adds the hook endpoint and the prompt endpoint to mux.
func (in *Inbox) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /hooks/stop", in.serveHook)
	mux.HandleFunc("POST /prompts/{session}", in.servePrompt)
}

// hookInput is the part of the Stop hook input the inbox uses.
type hookInput struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
}

// HookAnswer is the hook endpoint's reply when a viewer sent a prompt.
type HookAnswer struct {
	Prompt string `json:"prompt"`
}

const maxHookInput = 1 << 20

func (in *Inbox) serveHook(w http.ResponseWriter, r *http.Request) {
	var input hookInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxHookInput)).Decode(&input); err != nil {
		http.Error(w, "invalid hook input: "+err.Error(), http.StatusBadRequest)
		return
	}
	if input.HookEventName != "Stop" || input.SessionID == "" {
		http.Error(w, "expected a Stop hook with a session_id", http.StatusBadRequest)
		return
	}
	start := time.Now()
	text, outcome := in.Wait(r.Context(), Idle{SessionID: input.SessionID, Time: start, Cwd: input.Cwd})
	in.logger.Info("prompt",
		"session_id", input.SessionID,
		"outcome", string(outcome),
		"prompt_bytes", len(text),
		"wait_ms", time.Since(start).Milliseconds(),
	)
	if outcome != Prompted {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(HookAnswer{Prompt: text})
}

func (in *Inbox) servePrompt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxPrompt+1024)).Decode(&body); err != nil {
		http.Error(w, "invalid prompt: "+err.Error(), http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(body.Prompt)
	if text == "" || len(text) > MaxPrompt {
		http.Error(w, "prompt must be 1 to 65536 bytes", http.StatusBadRequest)
		return
	}
	if err := in.Send(r.PathValue("session"), text); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
