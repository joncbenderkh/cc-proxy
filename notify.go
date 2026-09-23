// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/joncbenderkh/cc-proxy/internal/approval"
	"github.com/joncbenderkh/cc-proxy/internal/prompt"
	"github.com/joncbenderkh/cc-proxy/internal/push"
)

// idleDelay is how long a session must wait for a prompt before a push
// says so; a reply read in the terminal within it needs no notification.
// It matches Claude Code's own idle_prompt notification.
const idleDelay = 60 * time.Second

// maxNotificationText bounds the body of a notification, in bytes.
const maxNotificationText = 240

// notifier pushes a notification when a permission prompt appears and
// when a session has waited idleDelay for its next prompt. Its methods
// are called with the broker's or inbox's lock held, so sends run in the
// background.
type notifier struct {
	send  func(push.Message)
	delay time.Duration

	mu        sync.Mutex
	approvals map[string]bool
	idle      map[string]time.Time
}

func newNotifier(send func(push.Message), delay time.Duration) *notifier {
	return &notifier{send: send, delay: delay, approvals: map[string]bool{}, idle: map[string]time.Time{}}
}

func (n *notifier) approvalsChanged(pending []approval.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	seen := make(map[string]bool, len(pending))
	for _, request := range pending {
		seen[request.ID] = true
		if !n.approvals[request.ID] {
			go n.send(approvalMessage(request))
		}
	}
	n.approvals = seen
}

func (n *notifier) idleChanged(idle []prompt.Idle) {
	n.mu.Lock()
	defer n.mu.Unlock()
	waiting := make(map[string]time.Time, len(idle))
	for _, entry := range idle {
		waiting[entry.SessionID] = entry.Time
		if !n.idle[entry.SessionID].Equal(entry.Time) {
			time.AfterFunc(n.delay, func() {
				if n.stillIdle(entry) {
					n.send(idleMessage(entry))
				}
			})
		}
	}
	n.idle = waiting
}

func (n *notifier) stillIdle(entry prompt.Idle) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	since, ok := n.idle[entry.SessionID]
	return ok && since.Equal(entry.Time)
}

func approvalMessage(request approval.Request) push.Message {
	return push.Message{
		Title:  project(request.Cwd) + ": allow " + request.ToolName + "?",
		Body:   clip(toolSummary(request.ToolInput)),
		Tag:    "approval-" + request.ID,
		URL:    sessionURL(request.SessionID),
		TTL:    10 * time.Minute,
		Urgent: true,
	}
}

func idleMessage(entry prompt.Idle) push.Message {
	body := strings.Join(strings.Fields(entry.Reply), " ")
	if body == "" {
		body = "Claude is waiting for your next prompt."
	}
	return push.Message{
		Title: project(entry.Cwd) + ": Claude is waiting",
		Body:  clip(body),
		Tag:   "idle-" + entry.SessionID,
		URL:   sessionURL(entry.SessionID),
		TTL:   time.Hour,
	}
}

func project(cwd string) string {
	if cwd = strings.TrimRight(cwd, `/\`); cwd == "" {
		return "Claude Code"
	}
	return path.Base(strings.ReplaceAll(cwd, `\`, "/"))
}

func sessionURL(sessionID string) string {
	if sessionID == "" {
		return "./"
	}
	return "./#s=" + url.PathEscape(sessionID)
}

// toolSummary picks the input field that says the most about a tool call,
// as the web page does.
func toolSummary(input json.RawMessage) string {
	var fields map[string]any
	if json.Unmarshal(input, &fields) == nil {
		for _, key := range []string{"command", "file_path", "pattern", "url", "query", "description", "prompt"} {
			if text, ok := fields[key].(string); ok {
				return text
			}
		}
	}
	return string(input)
}

func clip(text string) string {
	if len(text) <= maxNotificationText {
		return text
	}
	cut := maxNotificationText
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}
