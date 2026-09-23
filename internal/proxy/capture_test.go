// SPDX-License-Identifier: GPL-3.0-or-later

package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/andybalholm/brotli"
)

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(s))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func brotlied(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	bw := brotli.NewWriter(&buf)
	bw.Write([]byte(s))
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestLoggableBody(t *testing.T) {
	const message = `{"id":"msg_1","usage":{"input_tokens":3,"output_tokens":5}}`
	const stream = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		": keep-alive comment\n\n" +
		"event: ping\r\ndata: {\"type\": \"ping\"}\r\n\r\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\n" +
		"data: \"delta\":{\"text\":\"hi\"}}\n\n" +
		"event: error\ndata: not json\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}"

	tests := []struct {
		name   string
		body   []byte
		header http.Header
		want   string
	}{
		{"json", []byte(message), http.Header{"Content-Type": {"application/json"}}, message},
		{"plain text", []byte("upstream timeout"), http.Header{"Content-Type": {"text/plain"}}, `"upstream timeout"`},
		{"empty", nil, http.Header{}, `""`},
		{"gzip json", gzipped(t, message), http.Header{"Content-Encoding": {"gzip"}}, message},
		{"sse stream", []byte(stream), http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}},
			`[{"event":"message_start","data":{"type":"message_start"}},` +
				`{"event":"ping","data":{"type":"ping"}},` +
				`{"event":"content_block_delta","data":{"type":"content_block_delta","delta":{"text":"hi"}}},` +
				`{"event":"error","data":"not json"},` +
				`{"event":"message_stop","data":{"type":"message_stop"}}]`},
		{"brotli json", brotlied(t, message), http.Header{"Content-Encoding": {"br"}}, message},
		{"unsupported encoding", []byte{1, 2, 3}, http.Header{"Content-Encoding": {"zstd"}},
			`{"content_encoding":"zstd","bytes":3,"error":"unsupported content-encoding \"zstd\""}`},
		{"corrupt gzip", []byte("not gzip"), http.Header{"Content-Encoding": {"gzip"}},
			`{"content_encoding":"gzip","bytes":8,"error":"unexpected EOF"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(loggableBody(tt.body, tt.header))
			if err != nil {
				t.Fatal(err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, got); err != nil {
				t.Fatal(err)
			}
			if compact.String() != tt.want {
				t.Fatalf("got  %s\nwant %s", compact.String(), tt.want)
			}
		})
	}
}

func TestRedactCoversResponseCookies(t *testing.T) {
	got := redact(http.Header{"Set-Cookie": {"session=abc"}, "Request-Id": {"req_1"}})
	if got.Get("Set-Cookie") != "[REDACTED]" || got.Get("Request-Id") != "req_1" {
		t.Fatalf("redact = %v", got)
	}
}
