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
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joncbenderkh/cc-proxy/internal/proxy"
)

//go:embed VERSION
var rawVersion string

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cc-proxy:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("cc-proxy", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8787", "address to listen on")
	upstreamURL := flags.String("upstream", "https://api.anthropic.com", "Anthropic API base URL")
	showVersion := flags.Bool("version", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}

	version := strings.TrimSpace(rawVersion)
	if *showVersion {
		fmt.Println("cc-proxy", version)
		return nil
	}

	upstream, err := url.Parse(*upstreamURL)
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		return fmt.Errorf("invalid -upstream %q", *upstreamURL)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	server := &http.Server{
		Addr:              *listen,
		Handler:           proxy.New(upstream, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	logger.Info("listening", "version", version, "addr", *listen, "upstream", upstream.String())
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutdownDone
	return nil
}
