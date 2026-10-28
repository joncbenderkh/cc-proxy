// SPDX-License-Identifier: GPL-3.0-or-later

package sessionlog

import (
	"os"
	"path/filepath"
	"slices"
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
	dirs := []string{dir}

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
			got, ok := Find(dirs, tt.sessionID)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("Find(%q) = %+v, %v; want %+v, %v", tt.sessionID, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestFindNoDirs(t *testing.T) {
	dirs := []string{filepath.Join(t.TempDir(), "missing")}
	if _, ok := Find(dirs, "s1"); ok {
		t.Error("Find() = true for a nonexistent projects directory, want false")
	}
}

func TestFindAcrossMultipleDirs(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	dirs := []string{first, second}

	writeSession(t, second, "-home-u-proj", "s1", `
{"type":"user","message":{"role":"user","content":"fix the build"},"cwd":"/home/u/proj","gitBranch":"main"}
`)

	got, ok := Find(dirs, "s1")
	want := Session{Cwd: "/home/u/proj", Branch: "main", Title: "fix the build"}
	if !ok || got != want {
		t.Errorf("Find(%q) = %+v, %v; want %+v, true", "s1", got, ok, want)
	}
}

func TestFindFirstDirWins(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	dirs := []string{first, second}

	writeSession(t, first, "-home-u-a", "s1", `
{"type":"user","message":{"role":"user","content":"first"}}
`)
	writeSession(t, second, "-home-u-b", "s1", `
{"type":"user","message":{"role":"user","content":"second"}}
`)

	if got, _ := Find(dirs, "s1"); got.Title != "first" {
		t.Errorf("Find(%q).Title = %q, want %q", "s1", got.Title, "first")
	}
}

func TestFindTakesDirLiterally(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".claude-[work]", "projects")
	dirs := []string{dir}

	writeSession(t, dir, "-home-u-proj", "s1", `
{"type":"user","message":{"role":"user","content":"fix the build"}}
`)

	if _, ok := Find(dirs, "s1"); !ok {
		t.Errorf("Find(%q) = false for a directory name with glob characters, want true", "s1")
	}
}

func TestFindRejectsPatternSessionIDs(t *testing.T) {
	dir := t.TempDir()
	dirs := []string{dir}

	writeSession(t, dir, "-home-u-proj", "s1", `
{"type":"user","message":{"role":"user","content":"secret"}}
`)

	for _, id := range []string{"*", "s?", "[s]1", "../-home-u-proj/s1"} {
		if got, ok := Find(dirs, id); ok {
			t.Errorf("Find(%q) = %+v, true; want false", id, got)
		}
	}
}

func TestFindFollowsSymlinkedProjects(t *testing.T) {
	dir, target := t.TempDir(), t.TempDir()
	dirs := []string{dir}

	writeSession(t, target, "proj", "s1", `
{"type":"user","message":{"role":"user","content":"fix the build"}}
`)
	if err := os.Symlink(filepath.Join(target, "proj"), filepath.Join(dir, "-home-u-proj")); err != nil {
		t.Fatal(err)
	}

	if _, ok := Find(dirs, "s1"); !ok {
		t.Errorf("Find(%q) = false for a symlinked project directory, want true", "s1")
	}
}

func TestDefaultDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	work := t.TempDir()
	t.Chdir(work)
	claude := filepath.Join(home, ".claude", "projects")
	for _, test := range []struct {
		name, configDir string
		want            []string
	}{
		{"unset", "", []string{claude}},
		{"other directory", filepath.Join(work, "claude-work"), []string{claude, filepath.Join(work, "claude-work", "projects")}},
		{"relative", "claude-work", []string{claude, filepath.Join(work, "claude-work", "projects")}},
		{"default directory", filepath.Join(home, ".claude"), []string{claude}},
		{"default directory, trailing slash", filepath.Join(home, ".claude") + string(filepath.Separator), []string{claude}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", test.configDir)
			if got := DefaultDirs(); !slices.Equal(got, test.want) {
				t.Errorf("DefaultDirs() = %q, want %q", got, test.want)
			}
		})
	}
}
