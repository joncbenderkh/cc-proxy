// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/joncbenderkh/cc-proxy/internal/approval"
	"github.com/joncbenderkh/cc-proxy/internal/prompt"
	"github.com/joncbenderkh/cc-proxy/internal/push"
)

func recordingNotifier(delay time.Duration) (*notifier, <-chan push.Message) {
	sent := make(chan push.Message, 8)
	return newNotifier(func(m push.Message) { sent <- m }, delay), sent
}

func expectMessage(t *testing.T, sent <-chan push.Message) push.Message {
	t.Helper()
	select {
	case m := <-sent:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no notification sent")
		return push.Message{}
	}
}

func expectNone(t *testing.T, sent <-chan push.Message, wait time.Duration) {
	t.Helper()
	select {
	case m := <-sent:
		t.Fatalf("unexpected notification %+v", m)
	case <-time.After(wait):
	}
}

func TestNotifiesEachNewApprovalOnce(t *testing.T) {
	n, sent := recordingNotifier(time.Hour)
	request := approval.Request{ID: "A1", SessionID: "s 1", Cwd: "/work/cc-proxy", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"go test ./..."}`)}
	n.approvalsChanged([]approval.Request{request})
	m := expectMessage(t, sent)
	if m.Title != "cc-proxy: allow Bash?" || m.Body != "go test ./..." || m.Tag != "approval-A1" || m.URL != "./#s=s%201" || !m.Urgent {
		t.Fatalf("message = %+v", m)
	}
	n.approvalsChanged([]approval.Request{request})
	n.approvalsChanged(nil)
	expectNone(t, sent, 50*time.Millisecond)
}

func TestNotifiesIdleSessionAfterDelay(t *testing.T) {
	n, sent := recordingNotifier(20 * time.Millisecond)
	start := time.Now()
	answered := prompt.Idle{SessionID: "s1", Time: start, Cwd: "/work/a"}
	n.idleChanged([]prompt.Idle{answered})
	n.idleChanged(nil)
	expectNone(t, sent, 100*time.Millisecond)

	waiting := prompt.Idle{SessionID: "s2", Time: start, Cwd: "/work/b", Reply: "All tests\npass. " + strings.Repeat("x", 400)}
	n.idleChanged([]prompt.Idle{waiting})
	m := expectMessage(t, sent)
	if m.Title != "b: Claude is waiting" || !strings.HasPrefix(m.Body, "All tests pass. xxx") || len(m.Body) > maxNotificationText+len("…") || m.Tag != "idle-s2" {
		t.Fatalf("message = %+v", m)
	}
	expectNone(t, sent, 50*time.Millisecond)
}
