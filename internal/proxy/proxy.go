// SPDX-License-Identifier: GPL-3.0-or-later

// Package proxy implements the transparent reverse proxy that sits between
// Claude Code and the Anthropic API.
package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// New returns a handler that forwards every request to upstream unchanged,
// relays responses (including SSE streams) without buffering, and logs one
// record per exchange. Credential header values are never logged.
func New(upstream *url.URL, logger *slog.Logger) http.Handler {
	reverseProxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("upstream request failed", "method", r.Method, "path", r.URL.Path, "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return logExchanges(reverseProxy, logger)
}

func logExchanges(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		logger.Info("exchange",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"bytes", recorder.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", recorder.Header().Get("Request-Id"),
		)
	})
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
