// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"os"
	"path/filepath"
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
	tokenFile := filepath.Join(t.TempDir(), "ui-token")
	if err := os.WriteFile(tokenFile, []byte("abcdefghijklmnopqrstuvwxyz234567\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
		{"pretty with value", []string{"--pretty=yes"}, "invalid argument"},
		{"upstream without host", []string{"--upstream", "https://"}, "missing host"},
		{"ui-listen not loopback", []string{"--ui-listen", "0.0.0.0:8788"}, "must be a loopback address"},
		{"ui-listen all interfaces", []string{"--ui-listen", ":8788"}, "must be a loopback address"},
		{"ui-listen without port", []string{"--ui-listen", "localhost"}, "invalid --ui-listen"},
		{"ui-token-file without ui-listen", []string{"--ui-token-file", "token"}, "--ui-token-file requires --ui-listen"},
		{"history-file without ui-listen", []string{"--history-file", "turns.jsonl"}, "--history-file requires --ui-listen"},
		{"history-file unusable", []string{"--ui-listen", "127.0.0.1:0", "--ui-token-file", tokenFile, "--history-file", "/dev/null/turns.jsonl"}, "invalid --history-file"},
		{"ui-token-file unusable", []string{"--ui-listen", "127.0.0.1:0", "--ui-token-file", "/dev/null/ui-token"}, "invalid --ui-token-file"},
		{"claude-config-dir without ui-listen", []string{"--claude-config-dir", "/tmp/x"}, "--claude-config-dir requires --ui-listen"},
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
	for _, flag := range []string{"--listen", "--upstream", "--log-requests", "--log-responses", "--pretty", "--ui-listen", "--ui-token-file", "--history-file", "--claude-config-dir", "--version", "--help"} {
		if !strings.Contains(out, flag) {
			t.Errorf("help missing %s:\n%s", flag, out)
		}
	}
}

func TestIndentWriterPrettyPrintsRecords(t *testing.T) {
	tests := []struct {
		name, record, want string
	}{
		{"json record", `{"msg":"exchange","request_body":{"model":"claude-sonnet-5"}}` + "\n",
			"{\n  \"msg\": \"exchange\",\n  \"request_body\": {\n    \"model\": \"claude-sonnet-5\"\n  }\n}\n"},
		{"non-json passes through", "not json\n", "not json\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			n, err := indentWriter{&out}.Write([]byte(tt.record))
			if err != nil || n != len(tt.record) {
				t.Fatalf("Write = %d, %v", n, err)
			}
			if out.String() != tt.want {
				t.Fatalf("output = %q, want %q", out.String(), tt.want)
			}
		})
	}
}

func TestLoggerSendsOnlyErrorsToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	logger := newLogger(&stdout, &stderr, false).With("component", "proxy")
	logger.Info("exchange")
	logger.Warn("slow upstream")
	logger.Error("upstream request failed")

	if got := strings.Count(stdout.String(), "\n"); got != 2 || strings.Contains(stdout.String(), "upstream request failed") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, `"msg":"upstream request failed"`) || !strings.Contains(got, `"component":"proxy"`) {
		t.Errorf("stderr = %q", got)
	}
}
