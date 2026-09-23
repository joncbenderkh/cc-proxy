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
	if mediaType, _, _ := mime.ParseMediaType(header.Get("Content-Type")); mediaType == "text/event-stream" {
		return parseSSE(decoded)
	}
	return jsonOrString(decoded)
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

func parseSSE(stream []byte) []sseEvent {
	events := []sseEvent{}
	var name string
	var data []string
	dispatch := func() {
		if len(data) > 0 {
			events = append(events, sseEvent{Event: name, Data: jsonOrString([]byte(strings.Join(data, "\n")))})
		}
		name, data = "", nil
	}
	for _, line := range strings.Split(string(stream), "\n") {
		line = strings.TrimSuffix(line, "\r")
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch {
		case line == "":
			dispatch()
		case field == "event":
			name = value
		case field == "data":
			data = append(data, value)
		}
	}
	dispatch()
	return events
}

func jsonOrString(body []byte) any {
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	return string(body)
}
