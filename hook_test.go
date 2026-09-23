// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const hookTestToken = "abcdefghijklmnopqrstuvwxyz234567"

// runStopHook runs `cc-proxy hook stop` against a server answering with
// status and body, and returns its stdout, stderr and error.
func runStopHook(t *testing.T, status int, body string) (string, string, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/hooks/stop" || r.Header.Get("Authorization") != "Bearer "+hookTestToken || string(input) != `{"hook_event_name":"Stop"}` {
			http.Error(w, "unexpected request", http.StatusTeapot)
			return
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	tokenFile := filepath.Join(t.TempDir(), "ui-token")
	if err := os.WriteFile(tokenFile, []byte(hookTestToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetIn(strings.NewReader(`{"hook_event_name":"Stop"}`))
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"hook", "stop", "--ui-url", server.URL, "--ui-token-file", tokenFile})
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestStopHookWakesClaudeWithPrompt(t *testing.T) {
	stdout, stderr, err := runStopHook(t, http.StatusOK, `{"prompt":"run the tests"}`)
	if !errors.Is(err, errWakeClaude) {
		t.Fatalf("err = %v, want errWakeClaude", err)
	}
	if want := promptPreamble + "\n\nrun the tests\n"; stderr != want || stdout != "" {
		t.Fatalf("stdout %q, stderr %q; want stderr %q", stdout, stderr, want)
	}
}

func TestStopHookWithoutPromptExitsQuietly(t *testing.T) {
	stdout, stderr, err := runStopHook(t, http.StatusNoContent, "")
	if err != nil || stdout != "" || stderr != "" {
		t.Fatalf("err %v, stdout %q, stderr %q", err, stdout, stderr)
	}
}

func TestStopHookReportsServerErrors(t *testing.T) {
	_, _, err := runStopHook(t, http.StatusUnauthorized, "unauthorized")
	if err == nil || errors.Is(err, errWakeClaude) || !strings.Contains(err.Error(), "401 Unauthorized: unauthorized") {
		t.Fatalf("err = %v", err)
	}
}

func TestStopHookRejectsInvalidFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"ui-url without scheme", []string{"--ui-url", "localhost:8788"}, "scheme must be http or https"},
		{"ui-url without host", []string{"--ui-url", "http://"}, "missing host"},
		{"missing token file", []string{"--ui-token-file", "/nonexistent/ui-token"}, "invalid --ui-token-file"},
		{"positional argument", []string{"now"}, "unknown command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := execute(append([]string{"hook", "stop"}, tt.args...)...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
