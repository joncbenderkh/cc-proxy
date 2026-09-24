// SPDX-License-Identifier: GPL-3.0-or-later

package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSession(t *testing.T, dir, project, sessionID, content string) {
	t.Helper()
	path := filepath.Join(dir, project, sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimLeft(content, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	Dir = dir
	t.Cleanup(func() { Dir = defaultDir() })

	writeSession(t, dir, "-home-u-proj", "s1", `
{"type":"user","message":{"role":"user","content":"<local-command-caveat>ignored</local-command-caveat>"},"isMeta":true,"cwd":"/home/u/proj","gitBranch":"main"}
{"type":"user","message":{"role":"user","content":"fix the build\nplease"},"cwd":"/home/u/proj","gitBranch":"main"}
{"type":"attachment","attachment":{"type":"session_context","context":{"userEmail":"The user's email address is u@example.com. Use it only to identify the user."}},"cwd":"/home/u/proj","gitBranch":"main"}
`)
	writeSession(t, dir, "-home-u-other", "s2", `
{"type":"user","message":{"role":"user","content":"hi"}}
`)

	tests := []struct {
		name      string
		sessionID string
		want      Session
		wantOK    bool
	}{
		{"full session", "s1", Session{Cwd: "/home/u/proj", Branch: "main", Title: "fix the build", User: "u@example.com"}, true},
		{"partial session", "s2", Session{Title: "hi"}, true},
		{"unknown session", "missing", Session{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Find(tt.sessionID)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("Find(%q) = %+v, %v; want %+v, %v", tt.sessionID, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestFindNoDir(t *testing.T) {
	Dir = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { Dir = defaultDir() })
	if _, ok := Find("s1"); ok {
		t.Error("Find() = true for a nonexistent projects directory, want false")
	}
}
