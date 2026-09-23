// SPDX-License-Identifier: GPL-3.0-or-later

package feed

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func ids(events []event) []int64 {
	var out []int64
	for _, ev := range events {
		out = append(out, ev.id)
	}
	return out
}

func TestHubKeepsBoundedHistory(t *testing.T) {
	hub := NewHub(2)
	for range 3 {
		hub.Publish(Turn{})
	}
	tests := []struct {
		after int64
		want  []int64
	}{
		{0, []int64{2, 3}},
		{2, []int64{3}},
		{3, nil},
	}
	for _, tt := range tests {
		backlog, _, cancel := hub.subscribe(tt.after)
		cancel()
		if got := ids(backlog); !slices.Equal(got, tt.want) {
			t.Errorf("after %d: backlog %v, want %v", tt.after, got, tt.want)
		}
	}
}

func TestRestoredTurnsReplayAndNumberingContinues(t *testing.T) {
	hub := NewHub(2)
	hub.Restore([]Turn{{Seq: 7, SessionID: "a"}, {Seq: 8, SessionID: "b"}, {Seq: 9, SessionID: "c"}})
	turn, ok := hub.Publish(Turn{SessionID: "d"})
	if !ok || turn.Seq != 10 {
		t.Fatalf("published seq %d, ok %v; want 10", turn.Seq, ok)
	}
	backlog, _, cancel := hub.subscribe(8)
	defer cancel()
	var ids []int64
	for _, ev := range backlog {
		ids = append(ids, ev.id)
	}
	if len(ids) != 2 || ids[0] != 9 || ids[1] != 10 {
		t.Fatalf("backlog ids = %v, want [9 10]", ids)
	}
	hub.Close()
	if _, ok := hub.Publish(Turn{}); ok {
		t.Fatal("publish after close reported ok")
	}
}

func TestHubDisconnectsSlowSubscriber(t *testing.T) {
	hub := NewHub(1)
	_, events, cancel := hub.subscribe(0)
	defer cancel()
	for range subscriberBuffer + 1 {
		hub.Publish(Turn{})
	}
	received := 0
	for range events {
		received++
	}
	if received != subscriberBuffer {
		t.Fatalf("received %d turns before disconnect, want %d", received, subscriberBuffer)
	}
}

func readEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var event strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read event: %v (got %q)", err, event.String())
		}
		if line == "\n" {
			return event.String()
		}
		event.WriteString(line)
	}
}

func TestEventsReplayThenStreamLive(t *testing.T) {
	hub := NewHub(10)
	hub.Publish(Turn{SessionID: "s1", Status: 200})
	hub.Publish(Turn{SessionID: "s2", Status: 200})
	server := httptest.NewServer(hub.Handler())
	defer server.Close()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/events", nil)
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	events := bufio.NewReader(resp.Body)
	if got := readEvent(t, events); !strings.HasPrefix(got, "id: 2\nevent: turn\ndata: {\"seq\":2,") || !strings.Contains(got, `"session_id":"s2"`) {
		t.Fatalf("replayed event = %q", got)
	}

	hub.Publish(Turn{SessionID: "s3", Status: 200})
	live := make(chan string, 1)
	go func() { live <- readEvent(t, events) }()
	select {
	case got := <-live:
		if !strings.HasPrefix(got, "id: 3\n") {
			t.Fatalf("live event = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live event not delivered")
	}

	hub.Close()
	if rest, _ := io.ReadAll(events); len(rest) != 0 {
		t.Fatalf("data after close: %q", rest)
	}
}

func TestStateIsSentOnConnectAndOnChange(t *testing.T) {
	hub := NewHub(10)
	hub.Publish(Turn{})
	hub.SetState("approvals", []string{"a"})
	server := httptest.NewServer(hub.Handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := bufio.NewReader(resp.Body)
	if got := readEvent(t, events); !strings.HasPrefix(got, "id: 1\nevent: turn\n") {
		t.Fatalf("first event = %q", got)
	}
	if got, want := readEvent(t, events), "event: approvals\ndata: [\"a\"]\n"; got != want {
		t.Fatalf("state snapshot = %q, want %q", got, want)
	}
	if hub.Viewers() != 1 {
		t.Fatalf("viewers = %d, want 1", hub.Viewers())
	}
	hub.SetState("approvals", []string{})
	if got, want := readEvent(t, events), "event: approvals\ndata: []\n"; got != want {
		t.Fatalf("state change = %q, want %q", got, want)
	}
}

func TestIndexPage(t *testing.T) {
	server := httptest.NewServer(NewHub(1).Handler())
	defer server.Close()
	tests := []struct {
		path   string
		status int
	}{
		{"/", http.StatusOK},
		{"/elsewhere", http.StatusNotFound},
	}
	for _, tt := range tests {
		resp, err := http.Get(server.URL + tt.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tt.status {
			t.Errorf("%s: status %d, want %d", tt.path, resp.StatusCode, tt.status)
		}
		if tt.status == http.StatusOK && !strings.Contains(string(body), `new EventSource("events")`) {
			t.Errorf("%s: page does not subscribe to events", tt.path)
		}
	}
}
