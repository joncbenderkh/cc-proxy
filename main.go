// SPDX-License-Identifier: GPL-3.0-or-later
//
// cc-proxy — a local reverse proxy that records Claude Code API traffic.
// Copyright (C) 2026 Jon Bender
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. See the LICENSE file for details.

package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/joncbenderkh/cc-proxy/internal/approval"
	"github.com/joncbenderkh/cc-proxy/internal/auth"
	"github.com/joncbenderkh/cc-proxy/internal/feed"
	"github.com/joncbenderkh/cc-proxy/internal/prompt"
	"github.com/joncbenderkh/cc-proxy/internal/proxy"
)

//go:embed VERSION
var rawVersion string

// feedHistory is how many turns the live feed replays to a new viewer.
const feedHistory = 500

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRootCommand().ExecuteContext(ctx); err != nil {
		stop()
		if errors.Is(err, errWakeClaude) {
			os.Exit(wakeExitCode)
		}
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	var listen, uiListen, uiTokenFile, upstreamURL string
	var logRequests, logResponses, pretty bool
	cmd := &cobra.Command{
		Use:     "cc-proxy",
		Short:   "Local reverse proxy that records Claude Code API traffic",
		Version: strings.TrimSpace(rawVersion),
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateListen("--listen", listen); err != nil {
				return err
			}
			if uiListen != "" {
				if err := validateLoopback("--ui-listen", uiListen); err != nil {
					return err
				}
			} else if cmd.Flags().Changed("ui-token-file") {
				return errors.New("--ui-token-file requires --ui-listen")
			}
			upstream, err := parseUpstream(upstreamURL)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			logger := newLogger(cmd.OutOrStdout(), cmd.ErrOrStderr(), pretty)
			var token string
			if uiListen != "" {
				if token, err = loadUIToken(uiTokenFile, logger); err != nil {
					return err
				}
			}
			return serve(cmd.Context(), listen, uiListen, token, upstream, logger, cmd.Version, proxy.Options{LogRequests: logRequests, LogResponses: logResponses})
		},
	}
	cmd.SetVersionTemplate("cc-proxy {{.Version}}\n")
	cmd.AddCommand(newHookCommand())
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8787", "address to listen on (host:port)")
	cmd.Flags().StringVar(&uiListen, "ui-listen", "", "serve the live feed web page on this loopback address (host:port); off when empty")
	cmd.Flags().StringVar(&uiTokenFile, "ui-token-file", "", "file holding the web page login token, created if missing (default <user config dir>/cc-proxy/ui-token)")
	cmd.Flags().StringVar(&upstreamURL, "upstream", "https://api.anthropic.com", "Anthropic API base URL (http or https)")
	cmd.Flags().BoolVar(&logRequests, "log-requests", false, "log the headers and body of every request sent upstream (credentials redacted)")
	cmd.Flags().BoolVar(&logResponses, "log-responses", false, "log the headers and body of every response relayed to the client")
	cmd.Flags().BoolVar(&pretty, "pretty", false, "pretty-print log records as indented JSON")
	return cmd
}

// newLogger writes records to stdout, except errors, which go to stderr.
func newLogger(stdout, stderr io.Writer, pretty bool) *slog.Logger {
	if pretty {
		stdout, stderr = indentWriter{stdout}, indentWriter{stderr}
	}
	return slog.New(levelSplitHandler{
		below: slog.NewJSONHandler(stdout, nil),
		above: slog.NewJSONHandler(stderr, nil),
		split: slog.LevelError,
	})
}

// levelSplitHandler sends records at or above split to one handler and
// everything else to another.
type levelSplitHandler struct {
	below, above slog.Handler
	split        slog.Level
}

func (h levelSplitHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= h.split {
		return h.above.Enabled(ctx, level)
	}
	return h.below.Enabled(ctx, level)
}

func (h levelSplitHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level >= h.split {
		return h.above.Handle(ctx, record)
	}
	return h.below.Handle(ctx, record)
}

func (h levelSplitHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return levelSplitHandler{below: h.below.WithAttrs(attrs), above: h.above.WithAttrs(attrs), split: h.split}
}

func (h levelSplitHandler) WithGroup(name string) slog.Handler {
	return levelSplitHandler{below: h.below.WithGroup(name), above: h.above.WithGroup(name), split: h.split}
}

// indentWriter re-indents each JSON log record written through it. The
// slog JSON handler emits one complete record per Write call.
type indentWriter struct {
	out io.Writer
}

func (w indentWriter) Write(record []byte) (int, error) {
	var indented bytes.Buffer
	if err := json.Indent(&indented, record, "", "  "); err != nil {
		return w.out.Write(record)
	}
	if _, err := w.out.Write(indented.Bytes()); err != nil {
		return 0, err
	}
	return len(record), nil
}

func validateListen(flag, listen string) error {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", flag, listen, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("invalid %s %q: port must be 0-65535", flag, listen)
	}
	return nil
}

// validateLoopback accepts only loopback addresses: the feed is plain HTTP,
// so a login token sent to any other address could be read on the wire.
// Reach it remotely through a TLS tunnel such as `tailscale serve`.
func validateLoopback(flag, listen string) error {
	if err := validateListen(flag, listen); err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(listen)
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("invalid %s %q: must be a loopback address such as 127.0.0.1", flag, listen)
	}
	return nil
}

// loadUIToken reads or creates the web page login token. The token itself
// is never logged, only the file that holds it.
func loadUIToken(path string, logger *slog.Logger) (string, error) {
	if path == "" {
		var err error
		if path, err = auth.DefaultTokenFile(); err != nil {
			return "", fmt.Errorf("locate --ui-token-file: %w", err)
		}
	}
	token, created, err := auth.LoadOrCreateToken(path)
	if err != nil {
		return "", fmt.Errorf("invalid --ui-token-file: %w", err)
	}
	logger.Info("ui token", "file", path, "created", created)
	return token, nil
}

func parseUpstream(raw string) (*url.URL, error) {
	upstream, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid --upstream %q: %w", raw, err)
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return nil, fmt.Errorf("invalid --upstream %q: scheme must be http or https", raw)
	}
	if upstream.Host == "" {
		return nil, fmt.Errorf("invalid --upstream %q: missing host", raw)
	}
	return upstream, nil
}

func serve(ctx context.Context, listen, uiListen, uiToken string, upstream *url.URL, logger *slog.Logger, version string, opts proxy.Options) error {
	var servers []*http.Server
	if uiListen != "" {
		hub := feed.NewHub(feedHistory)
		opts.OnTurn = hub.Publish
		broker := approval.NewBroker(hub.Viewers, func(pending []approval.Request) { hub.SetState("approvals", pending) }, logger)
		mux := http.NewServeMux()
		mux.Handle("/", hub.Handler())
		broker.Register(mux)
		prompt.NewInbox(func(idle []prompt.Idle) { hub.SetState("idle", idle) }, logger).Register(mux)
		// Canceling the base context on shutdown releases held hooks, which
		// hands permission prompts back to the terminal.
		uiCtx, cancelUI := context.WithCancel(context.Background())
		ui := &http.Server{
			Addr:              uiListen,
			Handler:           auth.Require(uiToken, mux),
			ReadHeaderTimeout: 10 * time.Second,
			BaseContext:       func(net.Listener) context.Context { return uiCtx },
		}
		ui.RegisterOnShutdown(hub.Close)
		ui.RegisterOnShutdown(cancelUI)
		servers = append(servers, ui)
	}
	servers = append([]*http.Server{{
		Addr:              listen,
		Handler:           proxy.New(upstream, logger, opts),
		ReadHeaderTimeout: 10 * time.Second,
	}}, servers...)

	listeners := make([]net.Listener, 0, len(servers))
	for _, server := range servers {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			for _, open := range listeners {
				open.Close()
			}
			return err
		}
		listeners = append(listeners, listener)
	}

	failed := make(chan error, len(servers))
	for i, server := range servers {
		go func() {
			if err := server.Serve(listeners[i]); !errors.Is(err, http.ErrServerClosed) {
				failed <- err
			}
		}()
	}
	logger.Info("listening", "version", version, "addr", listen, "ui_addr", uiListen, "upstream", upstream.String())

	var err error
	select {
	case <-ctx.Done():
	case err = <-failed:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		server.Shutdown(shutdownCtx)
	}
	return err
}
