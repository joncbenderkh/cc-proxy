// SPDX-License-Identifier: GPL-3.0-or-later

package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joncbenderkh/cc-proxy/internal/feed"
)

const secretKey = "sk-ant-test-secret"

func newProxy(t *testing.T, upstream http.Handler, opts Options) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	front := httptest.NewServer(New(target, slog.New(slog.NewJSONHandler(&logs, nil)), opts))
	t.Cleanup(front.Close)
	return front, &logs
}

func TestForwardsRequestAndResponseUnchanged(t *testing.T) {
	const requestBody = `{"model":"claude-sonnet-5","max_tokens":16}`
	const responseBody = `{"id":"msg_1","usage":{"input_tokens":3,"output_tokens":5}}`

	front, logs := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path != "/v1/messages":
			t.Errorf("path = %q", r.URL.Path)
		case string(body) != requestBody:
			t.Errorf("request body = %q", body)
		case r.Header.Get("X-Api-Key") != secretKey:
			t.Errorf("x-api-key not forwarded")
		}
		w.Header().Set("Request-Id", "req_123")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, responseBody)
	}), Options{})

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(requestBody))
	req.Header.Set("X-Api-Key", secretKey)
	req.Header.Set("Authorization", "Bearer "+secretKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	front.Close()

	if resp.StatusCode != http.StatusOK || string(got) != responseBody {
		t.Fatalf("response = %d %q", resp.StatusCode, got)
	}
	if strings.Contains(logs.String(), secretKey) {
		t.Errorf("credential leaked into logs: %s", logs)
	}
	if !strings.Contains(logs.String(), `"request_id":"req_123"`) {
		t.Errorf("request id missing from logs: %s", logs)
	}
}

func TestStreamsEventsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	front, _ := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	}), Options{LogRequests: true, LogResponses: true})
	defer close(release)

	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	firstLine := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		firstLine <- line
	}()
	select {
	case line := <-firstLine:
		if line != "event: message_start\n" {
			t.Fatalf("first line = %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first event was buffered instead of relayed")
	}
}

func TestUpstreamFailureReturnsBadGateway(t *testing.T) {
	var logs bytes.Buffer
	unreachable := &url.URL{Scheme: "http", Host: "127.0.0.1:1"}
	front := httptest.NewServer(New(unreachable, slog.New(slog.NewJSONHandler(&logs, nil)), Options{}))
	defer front.Close()

	resp, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestLogRequestsRecordsOutboundRequest(t *testing.T) {
	const requestBody = `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	front, logs := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != requestBody {
			t.Errorf("upstream body = %q", body)
		}
	}), Options{LogRequests: true})

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(requestBody))
	req.Header.Set("X-Api-Key", secretKey)
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	front.Close()

	var record struct {
		RequestHeaders http.Header     `json:"request_headers"`
		RequestBody    json.RawMessage `json:"request_body"`
	}
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatalf("decode log %q: %v", logs, err)
	}
	if strings.Contains(logs.String(), secretKey) {
		t.Errorf("credential leaked into logs: %s", logs)
	}
	if got := record.RequestHeaders.Get("X-Api-Key"); got != "[REDACTED]" {
		t.Errorf("x-api-key logged as %q", got)
	}
	if got := record.RequestHeaders.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Errorf("anthropic-version logged as %q", got)
	}
	if string(record.RequestBody) != requestBody {
		t.Errorf("request_body = %s", record.RequestBody)
	}
}

func TestRequestsNotLoggedByDefault(t *testing.T) {
	front, logs := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), Options{})
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{"secret":"prompt"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	front.Close()
	if strings.Contains(logs.String(), "_body") || strings.Contains(logs.String(), "prompt") {
		t.Errorf("request logged without -log-requests: %s", logs)
	}
}

func TestLogResponsesRecordsRelayedResponse(t *testing.T) {
	const stream = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	front, logs := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "session="+secretKey)
		io.WriteString(w, stream)
	}), Options{LogResponses: true})

	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	front.Close()

	if string(got) != stream {
		t.Fatalf("client received %q", got)
	}
	var record struct {
		ResponseHeaders http.Header `json:"response_headers"`
		ResponseBody    []sseEvent  `json:"response_body"`
		RequestBody     any         `json:"request_body"`
	}
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatalf("decode log %q: %v", logs, err)
	}
	if strings.Contains(logs.String(), secretKey) {
		t.Errorf("cookie leaked into logs: %s", logs)
	}
	if record.ResponseHeaders.Get("Content-Type") != "text/event-stream" {
		t.Errorf("response_headers = %v", record.ResponseHeaders)
	}
	if len(record.ResponseBody) != 2 || record.ResponseBody[1].Event != "message_stop" {
		t.Errorf("response_body = %+v", record.ResponseBody)
	}
	if record.RequestBody != nil {
		t.Errorf("request body logged without LogRequests: %v", record.RequestBody)
	}
}

func TestRecordsMessageUsage(t *testing.T) {
	const message = `{"type":"message","id":"msg_1","model":"claude-sonnet-5","stop_reason":"end_turn",` +
		`"usage":{"input_tokens":1000000,"output_tokens":0}}`
	const stream = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_2","model":"claude-sonnet-5","usage":{"input_tokens":1000000,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":100000}}` + "\n\n"

	tests := []struct {
		name        string
		path        string
		status      int
		contentType string
		body        []byte
		gzip        bool
		want        string
		wantErr     bool
	}{
		{"json", "/v1/messages", http.StatusOK, "application/json", []byte(message), false,
			`{"id":"msg_1","model":"claude-sonnet-5","stop_reason":"end_turn","usage":{"input_tokens":1000000,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"cost_usd":2}`, false},
		{"gzip sse with query", "/v1/messages?beta=true", http.StatusOK, "text/event-stream", []byte(stream), true,
			`{"id":"msg_2","model":"claude-sonnet-5","stop_reason":"tool_use","usage":{"input_tokens":1000000,"output_tokens":100000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"cost_usd":3}`, false},
		{"unreadable body", "/v1/messages", http.StatusOK, "text/plain", []byte("oops"), false, "", true},
		{"error status", "/v1/messages", http.StatusTooManyRequests, "application/json", []byte(`{"type":"error"}`), false, "", false},
		{"count tokens", "/v1/messages/count_tokens", http.StatusOK, "application/json", []byte(`{"input_tokens":5}`), false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := tt.body
			if tt.gzip {
				body = gzipped(t, string(tt.body))
			}
			front, logs := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				if tt.gzip {
					w.Header().Set("Content-Encoding", "gzip")
				}
				w.WriteHeader(tt.status)
				w.Write(body)
			}), Options{})

			req, _ := http.NewRequest(http.MethodPost, front.URL+tt.path, strings.NewReader(`{}`))
			req.Header.Set("Accept-Encoding", "gzip")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			front.Close()

			if !bytes.Equal(got, body) {
				t.Fatalf("client received altered body %q", got)
			}
			var record struct {
				Message      json.RawMessage `json:"message"`
				MessageError string          `json:"message_error"`
			}
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatalf("decode log %q: %v", logs, err)
			}
			if string(record.Message) != tt.want {
				t.Errorf("message = %s\nwant      %s", record.Message, tt.want)
			}
			if (record.MessageError != "") != tt.wantErr {
				t.Errorf("message_error = %q, wantErr %v", record.MessageError, tt.wantErr)
			}
		})
	}
}

func TestOnTurnReceivesPromptReplyAndUsage(t *testing.T) {
	const stream = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi there"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n"

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	turns := make(chan feed.Turn, 2)
	front := httptest.NewServer(New(target, slog.New(slog.NewJSONHandler(io.Discard, nil)), Options{OnTurn: func(turn feed.Turn) { turns <- turn }}))
	defer front.Close()

	for _, path := range []string{"/v1/messages/count_tokens", "/v1/messages?beta=true"} {
		req, _ := http.NewRequest(http.MethodPost, front.URL+path, strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`))
		req.Header.Set("X-Claude-Code-Session-Id", "session-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	front.Close()
	close(turns)

	var got []feed.Turn
	for turn := range turns {
		got = append(got, turn)
	}
	if len(got) != 1 {
		t.Fatalf("published %d turns, want 1 (count_tokens excluded)", len(got))
	}
	turn := got[0]
	switch {
	case turn.SessionID != "session-1", turn.Status != http.StatusOK:
		t.Errorf("turn = %+v", turn)
	case len(turn.Prompt) != 1 || turn.Prompt[0].Text != "hello":
		t.Errorf("prompt = %+v", turn.Prompt)
	case len(turn.Reply) != 1 || turn.Reply[0].Text != "Hi there":
		t.Errorf("reply = %+v", turn.Reply)
	case turn.Message == nil || turn.Message.StopReason != "end_turn" || turn.Message.Usage.OutputTokens != 3:
		t.Errorf("message = %+v", turn.Message)
	}
}
