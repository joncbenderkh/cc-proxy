// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"strings"
	"testing"
)

func execute(args ...string) (string, error) {
	cmd := newRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"positional argument", []string{"serve"}, `unknown command "serve"`},
		{"single-dash long flag", []string{"-listen", ":8787"}, "unknown shorthand flag"},
		{"unknown flag", []string{"--port", "8787"}, "unknown flag: --port"},
		{"missing flag value", []string{"--listen"}, "flag needs an argument"},
		{"listen without port", []string{"--listen", "localhost"}, "invalid --listen"},
		{"listen port out of range", []string{"--listen", "127.0.0.1:99999"}, "port must be 0-65535"},
		{"listen port not numeric", []string{"--listen", "127.0.0.1:http"}, "port must be 0-65535"},
		{"upstream without scheme", []string{"--upstream", "api.anthropic.com"}, "scheme must be http or https"},
		{"upstream unsupported scheme", []string{"--upstream", "ftp://api.anthropic.com"}, "scheme must be http or https"},
		{"single-dash log-requests", []string{"-log-requests"}, "unknown shorthand flag"},
		{"log-requests with value", []string{"--log-requests=maybe"}, "invalid argument"},
		{"upstream without host", []string{"--upstream", "https://"}, "missing host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := execute(tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestVersionFlag(t *testing.T) {
	out, err := execute("--version")
	if err != nil {
		t.Fatal(err)
	}
	if want := "cc-proxy " + strings.TrimSpace(rawVersion) + "\n"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

func TestHelpUsesDoubleDashFlags(t *testing.T) {
	out, err := execute("--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--listen", "--upstream", "--log-requests", "--version", "--help"} {
		if !strings.Contains(out, flag) {
			t.Errorf("help missing %s:\n%s", flag, out)
		}
	}
}
