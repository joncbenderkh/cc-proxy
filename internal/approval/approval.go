// SPDX-License-Identifier: GPL-3.0-or-later

// Package approval lets a remote viewer answer Claude Code permission
// prompts. A PermissionRequest HTTP hook waits here until a viewer allows
// or denies the tool call. Claude Code shows its terminal dialog at the
// same time, so whichever answer comes first wins. Claude Code keeps the
// hook request open after an answer in the terminal, so the proxy reports
// the tool results each request sends upstream, and the broker withdraws
// the prompts they answer.
package approval

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/joncbenderkh/cc-proxy/internal/transcript"
)

// Request is a pending permission prompt as viewers see it.
type Request struct {
	ID        string          `json:"id"`
	Time      time.Time       `json:"time"`
	SessionID string          `json:"session_id,omitempty"`
	Cwd       string          `json:"cwd,omitempty"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	// Always holds the allow rules Claude Code suggested; allowing with
	// Decision.Always adds them, like "don't ask again" in the terminal.
	Always []json.RawMessage `json:"always,omitempty"`
}

// Decision is a viewer's answer to a Request.
type Decision struct {
	Behavior string `json:"behavior"`
	Always   bool   `json:"always,omitempty"`
}

// Outcome is how a wait ended.
type Outcome string

const (
	Allowed       Outcome = "allow"
	AllowedAlways Outcome = "allow_always"
	Denied        Outcome = "deny"
	Canceled      Outcome = "canceled"
	// AnsweredElsewhere means the tool call's result went upstream, so the
	// terminal dialog answered it first.
	AnsweredElsewhere Outcome = "answered_elsewhere"
)

// ErrNotPending is returned for a decision on a request that was already
// answered, abandoned, or never existed.
var ErrNotPending = errors.New("approval not pending")

// DeniedMessage tells Claude why a tool call was denied.
const DeniedMessage = "The user denied this tool call from the cc-proxy web page."

// Broker holds the pending permission prompts.
type Broker struct {
	publish func([]Request)
	logger  *slog.Logger

	mu      sync.Mutex
	pending []*waiter
}

type waiter struct {
	request  Request
	decision chan Decision
	answered chan struct{}
}

// NewBroker returns a broker that reports every change to the pending
// list through publish.
func NewBroker(publish func([]Request), logger *slog.Logger) *Broker {
	b := &Broker{publish: publish, logger: logger}
	b.publishLocked()
	return b
}

// Wait holds request until a viewer decides it or ctx ends.
func (b *Broker) Wait(ctx context.Context, request Request) (Decision, Outcome) {
	w := &waiter{request: request, decision: make(chan Decision, 1), answered: make(chan struct{})}
	b.mu.Lock()
	b.pending = append(b.pending, w)
	b.publishLocked()
	b.mu.Unlock()
	defer b.remove(w)

	select {
	case d := <-w.decision:
		switch {
		case d.Behavior == "deny":
			return d, Denied
		case d.Always:
			return d, AllowedAlways
		default:
			return d, Allowed
		}
	case <-w.answered:
		return Decision{}, AnsweredElsewhere
	case <-ctx.Done():
		return Decision{}, Canceled
	}
}

// Waiting reports whether the session has a pending request.
func (b *Broker) Waiting(sessionID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.ContainsFunc(b.pending, func(w *waiter) bool { return w.request.SessionID == sessionID })
}

// Answered withdraws the session's pending requests for the given tool
// calls, whose results have already gone upstream. A request matches by
// tool use id when the hook input carried one, else by tool name and input.
func (b *Broker) Answered(sessionID string, calls []transcript.ToolUse) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.pending[:0]
	for _, w := range b.pending {
		if w.request.SessionID == sessionID && slices.ContainsFunc(calls, w.request.answeredBy) {
			close(w.answered)
			continue
		}
		remaining = append(remaining, w)
	}
	if len(remaining) == len(b.pending) {
		return
	}
	clear(b.pending[len(remaining):])
	b.pending = remaining
	b.publishLocked()
}

func (r Request) answeredBy(call transcript.ToolUse) bool {
	if r.ToolUseID != "" {
		return r.ToolUseID == call.ID
	}
	return r.ToolName == call.Name && sameJSON(r.ToolInput, call.Input)
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// Decide answers the pending request with the given id.
func (b *Broker) Decide(id string, d Decision) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := slices.IndexFunc(b.pending, func(w *waiter) bool { return w.request.ID == id })
	if i < 0 {
		return ErrNotPending
	}
	b.pending[i].decision <- d
	b.pending = slices.Delete(b.pending, i, i+1)
	b.publishLocked()
	return nil
}

func (b *Broker) remove(w *waiter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if i := slices.Index(b.pending, w); i >= 0 {
		b.pending = slices.Delete(b.pending, i, i+1)
		b.publishLocked()
	}
}

func (b *Broker) publishLocked() {
	requests := make([]Request, 0, len(b.pending))
	for _, w := range b.pending {
		requests = append(requests, w.request)
	}
	b.publish(requests)
}

// Register adds the hook endpoint and the decision endpoint to mux.
func (b *Broker) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /hooks/permission-request", b.serveHook)
	mux.HandleFunc("POST /approvals/{id}", b.serveDecision)
}

// hookInput is the part of the PermissionRequest hook input the broker
// uses.
type hookInput struct {
	HookEventName         string            `json:"hook_event_name"`
	SessionID             string            `json:"session_id"`
	Cwd                   string            `json:"cwd"`
	ToolName              string            `json:"tool_name"`
	ToolInput             json.RawMessage   `json:"tool_input"`
	ToolUseID             string            `json:"tool_use_id"`
	PermissionSuggestions []json.RawMessage `json:"permission_suggestions"`
}

type hookOutput struct {
	HookSpecificOutput hookDecision `json:"hookSpecificOutput"`
}

type hookDecision struct {
	HookEventName string             `json:"hookEventName"`
	Decision      permissionDecision `json:"decision"`
}

type permissionDecision struct {
	Behavior           string            `json:"behavior"`
	UpdatedPermissions []json.RawMessage `json:"updatedPermissions,omitempty"`
	Message            string            `json:"message,omitempty"`
}

const maxHookInput = 1 << 20

func (b *Broker) serveHook(w http.ResponseWriter, r *http.Request) {
	var input hookInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxHookInput)).Decode(&input); err != nil {
		http.Error(w, "invalid hook input: "+err.Error(), http.StatusBadRequest)
		return
	}
	if input.HookEventName != "PermissionRequest" {
		http.Error(w, "expected a PermissionRequest hook", http.StatusBadRequest)
		return
	}
	request := Request{
		ID:        rand.Text(),
		Time:      time.Now(),
		SessionID: input.SessionID,
		Cwd:       input.Cwd,
		ToolName:  input.ToolName,
		ToolInput: input.ToolInput,
		ToolUseID: input.ToolUseID,
		Always:    allowRules(input.PermissionSuggestions),
	}
	start := time.Now()
	d, outcome := b.Wait(r.Context(), request)
	b.logger.Info("approval",
		"session_id", request.SessionID,
		"tool_name", request.ToolName,
		"outcome", string(outcome),
		"wait_ms", time.Since(start).Milliseconds(),
	)

	decision := permissionDecision{Behavior: d.Behavior}
	switch outcome {
	case Denied:
		decision.Message = DeniedMessage
	case AllowedAlways:
		decision.UpdatedPermissions = request.Always
	case Allowed:
	default:
		// Answered in the terminal, or shutting down: an empty answer
		// leaves the decision to the terminal dialog.
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(hookOutput{HookSpecificOutput: hookDecision{HookEventName: "PermissionRequest", Decision: decision}})
}

// allowRules keeps the suggestions that only add allow rules, so "always
// allow" never changes the permission mode or working directories.
func allowRules(suggestions []json.RawMessage) []json.RawMessage {
	var rules []json.RawMessage
	for _, suggestion := range suggestions {
		var entry struct{ Type, Behavior string }
		if json.Unmarshal(suggestion, &entry) == nil && entry.Type == "addRules" && entry.Behavior == "allow" {
			rules = append(rules, suggestion)
		}
	}
	return rules
}

func (b *Broker) serveDecision(w http.ResponseWriter, r *http.Request) {
	var d Decision
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&d); err != nil {
		http.Error(w, "invalid decision: "+err.Error(), http.StatusBadRequest)
		return
	}
	if d.Behavior != "allow" && d.Behavior != "deny" || d.Always && d.Behavior != "allow" {
		http.Error(w, `behavior must be "allow" or "deny"; always applies to allow only`, http.StatusBadRequest)
		return
	}
	if err := b.Decide(r.PathValue("id"), d); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
