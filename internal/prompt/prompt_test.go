// SPDX-License-Identifier: GPL-3.0-or-later

package prompt

import (
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
  "hook_event_name": "Stop",
  "stop_hook_active": false,
  "last_assistant_message": "Done."
}`

type fixture struct {
	inbox  *Inbox
	server *httptest.Server

	mu   sync.Mutex
	idle [][]Idle
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	f.inbox = NewInbox(func(idle []Idle) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.idle = append(f.idle, idle)
	}, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	f.inbox.Register(mux)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

type answer struct {
	status int
	body   string
}

// hook posts the hook input and returns the response once the hook is
// answered.
func (f *fixture) hook(t *testing.T, body string) <-chan answer {
	t.Helper()
	out := make(chan answer, 1)
	go func() {
		resp, err := http.Post(f.server.URL+"/hooks/stop", "application/json", strings.NewReader(body))
		if err != nil {
			out <- answer{body: "error: " + err.Error()}
			return
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		out <- answer{resp.StatusCode, strings.TrimSpace(string(data))}
	}()
	return out
}

// awaitIdle waits until the idle list has n sessions and returns it.
func (f *fixture) awaitIdle(t *testing.T, n int) []Idle {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		last := f.idle[len(f.idle)-1]
		f.mu.Unlock()
		if len(last) == n {
			return last
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("idle list never reached %d sessions", n)
	return nil
}

func (f *fixture) send(t *testing.T, session, body string) int {
	t.Helper()
	resp, err := http.Post(f.server.URL+"/prompts/"+session, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func receive(t *testing.T, ch <-chan answer) answer {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("hook not answered")
		return answer{}
	}
}

func TestPromptAnswersHook(t *testing.T) {
	f := newFixture(t)
	reply := f.hook(t, hookBody)
	idle := f.awaitIdle(t, 1)[0]
	if idle.SessionID != "s1" || idle.Cwd != "/work" {
		t.Fatalf("idle = %+v", idle)
	}
	if status := f.send(t, "s1", `{"prompt":"  run the tests \n"}`); status != http.StatusNoContent {
		t.Fatalf("send status %d", status)
	}
	if got := receive(t, reply); got.status != http.StatusOK || got.body != `{"prompt":"run the tests"}` {
		t.Fatalf("hook answer = %+v", got)
	}
	f.awaitIdle(t, 0)
	if status := f.send(t, "s1", `{"prompt":"again"}`); status != http.StatusConflict {
		t.Fatalf("second send status %d, want 409", status)
	}
}

func TestNewerStopSupersedesWait(t *testing.T) {
	f := newFixture(t)
	first := f.hook(t, hookBody)
	f.awaitIdle(t, 1)
	second := f.hook(t, strings.Replace(hookBody, "/work", "/work/sub", 1))
	if got := receive(t, first); got.status != http.StatusNoContent {
		t.Fatalf("first hook answer = %+v, want 204", got)
	}
	if idle := f.awaitIdle(t, 1)[0]; idle.Cwd != "/work/sub" {
		t.Fatalf("idle = %+v", idle)
	}
	f.send(t, "s1", `{"prompt":"next"}`)
	if got := receive(t, second); got.body != `{"prompt":"next"}` {
		t.Fatalf("second hook answer = %+v", got)
	}
}

func TestRejectsInvalidInput(t *testing.T) {
	f := newFixture(t)
	for _, body := range []string{`{"hook_event_name":"PermissionRequest","session_id":"s1"}`, `{"hook_event_name":"Stop"}`, `not json`} {
		resp, err := http.Post(f.server.URL+"/hooks/stop", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("hook %s: status %d, want 400", body, resp.StatusCode)
		}
	}
	for _, body := range []string{`{"prompt":"   "}`, `{"prompt":"` + strings.Repeat("x", MaxPrompt+1) + `"}`, `not json`} {
		if status := f.send(t, "s1", body); status != http.StatusBadRequest {
			t.Errorf("send %.40s: status %d, want 400", body, status)
		}
	}
	if status := f.send(t, "unknown", `{"prompt":"hi"}`); status != http.StatusConflict {
		t.Errorf("unknown session: status %d, want 409", status)
	}
}
