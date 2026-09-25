// SPDX-License-Identifier: GPL-3.0-or-later

// Package proxy implements the transparent reverse proxy that sits between
// Claude Code and the Anthropic API.
package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/joncbenderkh/cc-proxy/internal/feed"
	"github.com/joncbenderkh/cc-proxy/internal/gitrepo"
	"github.com/joncbenderkh/cc-proxy/internal/sessionlog"
	"github.com/joncbenderkh/cc-proxy/internal/transcript"
)

// Options tunes what the proxy records. The zero value logs exchange
// metadata only.
type Options struct {
	// LogRequests adds the headers and body of every request sent upstream
	// to its exchange record. Credential header values are redacted.
	LogRequests bool
	// LogResponses adds the headers and body of every response relayed to
	// the client. The body is copied as it is relayed, so streams are not
	// delayed; SSE streams are logged as a list of events.
	LogResponses bool
	// OnTurn, when set, receives every POST /v1/messages exchange with its
	// newest prompt, reply, usage and cost once the response has been
	// relayed in full.
	OnTurn func(feed.Turn)
	// OnRequestSent, when set, receives the session id and decoded body of
	// every POST /v1/messages request once it has been sent upstream in
	// full. It runs on the transport's goroutine, so it must not block.
	OnRequestSent func(sessionID string, request []byte)
}

// New returns a handler that forwards every request to upstream unchanged,
// relays responses (including SSE streams) without buffering, and logs one
// record per exchange. A successful POST /v1/messages exchange also records
// the model, token usage and cost the response reports. Credential header
// values are never logged.
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
	var seenSessions sync.Map
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sessionID := r.Header.Get("X-Claude-Code-Session-Id")
		createsMessage := r.Method == http.MethodPost && r.URL.Path == "/v1/messages"
		publishesTurn := createsMessage && opts.OnTurn != nil
		reportsSent := createsMessage && opts.OnRequestSent != nil
		if opts.OnTurn != nil && sessionID != "" {
			if _, seen := seenSessions.LoadOrStore(sessionID, struct{}{}); !seen {
				go backfillSession(opts.OnTurn, sessionID)
			}
		}
		var capture *requestCapture
		if opts.LogRequests || publishesTurn || reportsSent {
			capture = &requestCapture{}
			r = r.WithContext(context.WithValue(r.Context(), captureKey{}, capture))
		}
		if reportsSent {
			sessionID := r.Header.Get("X-Claude-Code-Session-Id")
			capture.onSent = func(body []byte) {
				if request, err := decode(body, capture.header.Get("Content-Encoding")); err == nil {
					opts.OnRequestSent(sessionID, request)
				}
			}
		}
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		if opts.LogResponses || createsMessage {
			recorder.body = &bytes.Buffer{}
		}
		next.ServeHTTP(recorder, r)
		duration := time.Since(start)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"bytes", recorder.bytes,
			"duration_ms", duration.Milliseconds(),
			"request_id", recorder.Header().Get("Request-Id"),
			"session_id", sessionID,
		}
		if opts.LogRequests {
			attrs = append(attrs, capture.attrs()...)
		}
		var response messageResponse
		if createsMessage {
			response = readMessageResponse(recorder.body.Bytes(), recorder.Header())
		}
		if createsMessage && recorder.status == http.StatusOK {
			attrs = append(attrs, response.attrs()...)
		}
		if publishesTurn {
			turn := feed.Turn{
				Time:       start,
				SessionID:  sessionID,
				Status:     recorder.status,
				DurationMs: duration.Milliseconds(),
				Reply:      response.reply,
			}
			if response.err == nil {
				turn.Message = &response.message
			}
			if request, err := decode(capture.body.Bytes(), capture.header.Get("Content-Encoding")); err == nil {
				turn.Prompt, _ = transcript.Prompt(request)
				if session, err := transcript.SessionOf(request); err == nil {
					turn.Cwd, turn.Title, turn.User = session.Cwd, session.Title, session.User
					if session.Cwd != "" {
						if repo, ok := gitrepo.Find(session.Cwd); ok {
							turn.Remote, turn.Branch = repo.Remote, repo.Branch
						}
					}
				}
			}
			opts.OnTurn(turn)
		}
		if opts.LogResponses {
			attrs = append(attrs,
				"response_headers", redact(recorder.Header()),
				"response_body", loggableBody(recorder.body.Bytes(), recorder.Header()),
			)
		}
		logger.Info("exchange", attrs...)
	})
}

// backfillSession publishes what Claude Code's own local transcript of
// sessionID already knows — cwd, branch, title, account email — as soon
// as the session is first seen, without waiting for one of its requests
// to complete. It runs off the request path since it reads a file from
// disk; a session with no local transcript, such as one authenticating
// with an API key rather than the Claude Code CLI, publishes nothing.
func backfillSession(onTurn func(feed.Turn), sessionID string) {
	session, ok := sessionlog.Find(sessionID)
	if !ok {
		return
	}
	turn := feed.Turn{
		Time:      time.Now(),
		SessionID: sessionID,
		Backfill:  true,
		Cwd:       session.Cwd,
		Branch:    session.Branch,
		Title:     session.Title,
		User:      session.User,
	}
	if session.Cwd != "" {
		if repo, ok := gitrepo.Find(session.Cwd); ok {
			turn.Remote = repo.Remote
		}
	}
	onTurn(turn)
}

type captureKey struct{}

// requestCapture holds a copy of the outbound request. The body is copied
// as the transport reads it, so forwarding is never delayed; onSent, when
// set, receives the copy once the transport has read the body to its end.
type requestCapture struct {
	header http.Header
	body   bytes.Buffer
	onSent func(body []byte)
}

func (c *requestCapture) record(out *http.Request) {
	c.header = redact(out.Header)
	if out.Body != nil && out.Body != http.NoBody {
		tee := &teeReadCloser{Reader: io.TeeReader(out.Body, &c.body), Closer: out.Body}
		if c.onSent != nil {
			tee.atEOF = func() { c.onSent(c.body.Bytes()) }
		}
		out.Body = tee
	}
}

func (c *requestCapture) attrs() []any {
	return []any{"request_headers", c.header, "request_body", loggableBody(c.body.Bytes(), c.header)}
}

type teeReadCloser struct {
	io.Reader
	io.Closer
	atEOF func()
	once  sync.Once
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.Reader.Read(p)
	if err == io.EOF && t.atEOF != nil {
		t.once.Do(t.atEOF)
	}
	return n, err
}

// statusRecorder captures the status code and body size of a response, and
// a copy of the body when body is non-nil.
// Unwrap lets http.ResponseController reach the underlying Flusher, so
// streaming responses are still flushed event by event.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	body   *bytes.Buffer
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	n, err := s.ResponseWriter.Write(p)
	s.bytes += int64(n)
	if s.body != nil {
		s.body.Write(p[:n])
	}
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
