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
	}), Options{LogRequests: true})
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
	if strings.Contains(logs.String(), "request_body") || strings.Contains(logs.String(), "prompt") {
		t.Errorf("request logged without -log-requests: %s", logs)
	}
}
