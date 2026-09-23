// SPDX-License-Identifier: GPL-3.0-or-later

package proxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"

	"github.com/joncbenderkh/cc-proxy/internal/sse"
	"github.com/joncbenderkh/cc-proxy/internal/transcript"
	"github.com/joncbenderkh/cc-proxy/internal/usage"
)

// redactedHeaders never have their values logged.
var redactedHeaders = []string{"X-Api-Key", "Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie"}

func redact(header http.Header) http.Header {
	clone := header.Clone()
	for _, name := range redactedHeaders {
		if _, ok := clone[name]; ok {
			clone[name] = []string{"[REDACTED]"}
		}
	}
	return clone
}

// undecodedBody stands in for a body whose Content-Encoding cannot be
// decoded for logging.
type undecodedBody struct {
	ContentEncoding string `json:"content_encoding"`
	Bytes           int    `json:"bytes"`
	Error           string `json:"error"`
}

// sseEvent is one server-sent event of a streaming response.
type sseEvent struct {
	Event string `json:"event,omitempty"`
	Data  any    `json:"data"`
}

// loggableBody renders a captured body for a log record: decoded per
// Content-Encoding, split into events for an SSE stream, embedded as JSON
// when valid, and a plain string otherwise.
func loggableBody(body []byte, header http.Header) any {
	encoding := header.Get("Content-Encoding")
	decoded, err := decode(body, encoding)
	if err != nil {
		return undecodedBody{ContentEncoding: encoding, Bytes: len(body), Error: err.Error()}
	}
	if isEventStream(header) {
		return loggableEvents(decoded)
	}
	return jsonOrString(decoded)
}

// messageResponse is what a Messages API response body reports: the
// model, usage and cost (or why they could not be read) and the reply.
type messageResponse struct {
	message usage.Message
	err     error
	reply   []transcript.Block
}

func readMessageResponse(body []byte, header http.Header) messageResponse {
	decoded, err := decode(body, header.Get("Content-Encoding"))
	if err != nil {
		return messageResponse{err: err}
	}
	var response messageResponse
	if isEventStream(header) {
		events := sse.Parse(decoded)
		response.message, response.err = usage.FromStream(events)
		response.reply, _ = transcript.ReplyStream(events)
	} else {
		response.message, response.err = usage.FromJSON(decoded)
		response.reply, _ = transcript.ReplyJSON(decoded)
	}
	return response
}

func (m messageResponse) attrs() []any {
	if m.err != nil {
		return []any{"message_error", m.err.Error()}
	}
	return []any{"message", m.message}
}

func isEventStream(header http.Header) bool {
	mediaType, _, _ := mime.ParseMediaType(header.Get("Content-Type"))
	return mediaType == "text/event-stream"
}

func decode(body []byte, encoding string) ([]byte, error) {
	var reader io.ReadCloser
	var err error
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, nil
	case "gzip":
		reader, err = gzip.NewReader(bytes.NewReader(body))
	case "deflate":
		reader, err = zlib.NewReader(bytes.NewReader(body))
	case "br":
		reader = io.NopCloser(brotli.NewReader(bytes.NewReader(body)))
	default:
		return nil, fmt.Errorf("unsupported content-encoding %q", encoding)
	}
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func loggableEvents(stream []byte) []sseEvent {
	events := []sseEvent{}
	for _, event := range sse.Parse(stream) {
		events = append(events, sseEvent{Event: event.Name, Data: jsonOrString([]byte(event.Data))})
	}
	return events
}

func jsonOrString(body []byte) any {
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	return string(body)
}
