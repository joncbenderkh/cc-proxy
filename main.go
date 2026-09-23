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
	"context"
	_ "embed"
	"errors"
	"fmt"
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

	"github.com/joncbenderkh/cc-proxy/internal/proxy"
)

//go:embed VERSION
var rawVersion string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRootCommand().ExecuteContext(ctx); err != nil {
		stop()
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	var listen, upstreamURL string
	cmd := &cobra.Command{
		Use:     "cc-proxy",
		Short:   "Local reverse proxy that records Claude Code API traffic",
		Version: strings.TrimSpace(rawVersion),
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateListen(listen); err != nil {
				return err
			}
			upstream, err := parseUpstream(upstreamURL)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			logger := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), nil))
			return serve(cmd.Context(), listen, upstream, logger, cmd.Version)
		},
	}
	cmd.SetVersionTemplate("cc-proxy {{.Version}}\n")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8787", "address to listen on (host:port)")
	cmd.Flags().StringVar(&upstreamURL, "upstream", "https://api.anthropic.com", "Anthropic API base URL (http or https)")
	return cmd
}

func validateListen(listen string) error {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid --listen %q: %w", listen, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("invalid --listen %q: port must be 0-65535", listen)
	}
	return nil
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

func serve(ctx context.Context, listen string, upstream *url.URL, logger *slog.Logger, version string) error {
	server := &http.Server{
		Addr:              listen,
		Handler:           proxy.New(upstream, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	logger.Info("listening", "version", version, "addr", listen, "upstream", upstream.String())
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutdownDone
	return nil
}
