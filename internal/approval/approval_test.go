// SPDX-License-Identifier: GPL-3.0-or-later

package approval

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const hookBody = `{
  "session_id": "s1",
  "cwd": "/work",
  "hook_event_name": "PermissionRequest",
  "tool_name": "Bash",
  "tool_input": {"command": "rm -rf node_modules"},
  "permission_suggestions": [
    {"type": "addRules", "rules": [{"toolName": "Bash", "ruleContent": "rm -rf node_modules"}], "behavior": "allow", "destination": "localSettings"},
    {"type": "setMode", "mode": "acceptEdits", "destination": "session"}
  ]
}`

type fixture struct {
	broker *Broker
	server *httptest.Server

	mu      sync.Mutex
	pending [][]Request
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	f.broker = NewBroker(func(r []Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.pending = append(f.pending, r)
	}, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	f.broker.Register(mux)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// hook posts the hook input with ctx and returns the response body once
// the hook is answered.
func (f *fixture) hook(t *testing.T, ctx context.Context, body string) <-chan string {
	t.Helper()
	out := make(chan string, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+"/hooks/permission-request", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		out <- string(data)
	}()
	return out
}

// awaitPending waits until n requests are pending and returns them.
func (f *fixture) awaitPending(t *testing.T, n int) []Request {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		last := f.pending[len(f.pending)-1]
		f.mu.Unlock()
		if len(last) == n {
			return last
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending list never reached %d requests", n)
	return nil
}

func (f *fixture) decide(t *testing.T, id, body string) int {
	t.Helper()
	resp, err := http.Post(f.server.URL+"/approvals/"+id, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func receive(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("hook not answered")
		return ""
	}
}

func TestViewerDecisionAnswersHook(t *testing.T) {
	tests := []struct {
		name, decision, want string
	}{
		{"allow", `{"behavior":"allow"}`,
			`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`},
		{"allow always keeps only allow rules", `{"behavior":"allow","always":true}`,
			`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow","updatedPermissions":[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"rm -rf node_modules"}],"behavior":"allow","destination":"localSettings"}]}}}`},
		{"deny", `{"behavior":"deny"}`,
			`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"` + DeniedMessage + `"}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			answer := f.hook(t, t.Context(), hookBody)
			pending := f.awaitPending(t, 1)[0]
			if pending.SessionID != "s1" || pending.ToolName != "Bash" || pending.Cwd != "/work" || len(pending.Always) != 1 {
				t.Fatalf("pending = %+v", pending)
			}
			if status := f.decide(t, pending.ID, tt.decision); status != http.StatusNoContent {
				t.Fatalf("decide status %d", status)
			}
			if got := receive(t, answer); strings.TrimSpace(got) != tt.want {
				t.Fatalf("hook answer = %s\nwant %s", got, tt.want)
			}
			if status := f.decide(t, pending.ID, tt.decision); status != http.StatusConflict {
				t.Fatalf("second decision status %d, want 409", status)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if last := f.pending[len(f.pending)-1]; len(last) != 0 {
				t.Fatalf("still pending: %+v", last)
			}
		})
	}
}

func TestTerminalAnswerWithdrawsRequest(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	f.hook(t, ctx, hookBody)
	pending := f.awaitPending(t, 1)[0]
	cancel()
	f.awaitPending(t, 0)
	if status := f.decide(t, pending.ID, `{"behavior":"allow"}`); status != http.StatusConflict {
		t.Fatalf("late decision status %d, want 409", status)
	}
}

func TestRejectsInvalidInput(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Post(f.server.URL+"/hooks/permission-request", "application/json", strings.NewReader(`{"hook_event_name":"PreToolUse"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("wrong hook event: status %d, want 400", resp.StatusCode)
	}
	for _, body := range []string{`{"behavior":"maybe"}`, `{"behavior":"deny","always":true}`, `not json`} {
		if status := f.decide(t, "unknown", body); status != http.StatusBadRequest {
			t.Errorf("decision %s: status %d, want 400", body, status)
		}
	}
	if status := f.decide(t, "unknown", `{"behavior":"allow"}`); status != http.StatusConflict {
		t.Errorf("unknown id: status %d, want 409", status)
	}
}

func TestPendingListIsNeverNull(t *testing.T) {
	f := newFixture(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	if data, _ := json.Marshal(f.pending[0]); string(data) != "[]" {
		t.Fatalf("initial pending = %s, want []", data)
	}
}
