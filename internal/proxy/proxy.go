// SPDX-License-Identifier: GPL-3.0-or-later

// Package proxy implements the transparent reverse proxy that sits between
// Claude Code and the Anthropic API.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// Options tunes what the proxy records. The zero value logs exchange
// metadata only.
type Options struct {
	// LogRequests adds the headers and body of every request sent upstream
	// to its exchange record. Credential header values are redacted.
	LogRequests bool
}

// redactedHeaders never have their values logged.
var redactedHeaders = []string{"X-Api-Key", "Authorization", "Proxy-Authorization", "Cookie"}

// New returns a handler that forwards every request to upstream unchanged,
// relays responses (including SSE streams) without buffering, and logs one
// record per exchange. Credential header values are never logged.
func New(upstream *url.URL, logger *slog.Logger, opts Options) http.Handler {
	reverseProxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			if capture, ok := r.In.Context().Value(captureKey{}).(*requestCapture); ok {
				capture.record(r.Out)
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("upstream request failed", "method", r.Method, "path", r.URL.Path, "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return logExchanges(reverseProxy, logger, opts)
}

func logExchanges(next http.Handler, logger *slog.Logger, opts Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var capture *requestCapture
		if opts.LogRequests {
			capture = &requestCapture{}
			r = r.WithContext(context.WithValue(r.Context(), captureKey{}, capture))
		}
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"bytes", recorder.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", recorder.Header().Get("Request-Id"),
		}
		if capture != nil {
			attrs = append(attrs, capture.attrs()...)
		}
		logger.Info("exchange", attrs...)
	})
}

type captureKey struct{}

// requestCapture holds a copy of the outbound request. The body is copied
// as the transport reads it, so forwarding is never delayed.
type requestCapture struct {
	header http.Header
	body   bytes.Buffer
}

func (c *requestCapture) record(out *http.Request) {
	c.header = redact(out.Header)
	if out.Body != nil && out.Body != http.NoBody {
		out.Body = teeReadCloser{Reader: io.TeeReader(out.Body, &c.body), Closer: out.Body}
	}
}

func (c *requestCapture) attrs() []any {
	body := c.body.Bytes()
	var loggedBody any = string(body)
	if json.Valid(body) {
		loggedBody = json.RawMessage(body)
	}
	return []any{"request_headers", c.header, "request_body", loggedBody}
}

func redact(header http.Header) http.Header {
	clone := header.Clone()
	for _, name := range redactedHeaders {
		if _, ok := clone[name]; ok {
			clone[name] = []string{"[REDACTED]"}
		}
	}
	return clone
}

type teeReadCloser struct {
	io.Reader
	io.Closer
}

// statusRecorder captures the status code and body size of a response.
// Unwrap lets http.ResponseController reach the underlying Flusher, so
// streaming responses are still flushed event by event.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	n, err := s.ResponseWriter.Write(p)
	s.bytes += int64(n)
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
