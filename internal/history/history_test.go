// SPDX-License-Identifier: GPL-3.0-or-later

package history

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/joncbenderkh/cc-proxy/internal/feed"
)

func seqs(turns []feed.Turn) []int64 {
	var out []int64
	for _, turn := range turns {
		out = append(out, turn.Seq)
	}
	return out
}

func lineCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(data, []byte("\n"))
}

func TestTurnsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "turns.jsonl")
	log, err := Open(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	for seq := range int64(4) {
		if err := log.Append(feed.Turn{Seq: seq + 1, SessionID: "s1", Title: "fix it"}); err != nil {
			t.Fatal(err)
		}
	}
	log.Close()
	if err := log.Append(feed.Turn{Seq: 5}); err != nil {
		t.Fatalf("append after close: %v", err)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("mode %v, want 0600", info.Mode().Perm())
		}
	}

	reopened, err := Open(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	turns := reopened.Turns()
	if got := seqs(turns); len(got) != 3 || got[0] != 2 || got[2] != 4 {
		t.Fatalf("seqs = %v, want [2 3 4]", got)
	}
	if turns[0].SessionID != "s1" || turns[0].Title != "fix it" {
		t.Errorf("turn = %+v", turns[0])
	}
}

func TestRewritesOnceTwiceTheKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turns.jsonl")
	log, err := Open(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	for seq := range int64(4) {
		log.Append(feed.Turn{Seq: seq + 1})
	}
	if n := lineCount(t, path); n != 4 {
		t.Fatalf("%d lines before rewrite, want 4", n)
	}
	log.Append(feed.Turn{Seq: 5})
	if n := lineCount(t, path); n != 2 {
		t.Fatalf("%d lines after rewrite, want 2", n)
	}
	log.Append(feed.Turn{Seq: 6})
	if got := seqs(log.Turns()); len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Fatalf("seqs = %v, want [5 6]", got)
	}
	if n := lineCount(t, path); n != 3 {
		t.Fatalf("%d lines after append, want 3", n)
	}
}

func TestSkipsDamagedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turns.jsonl")
	content := `{"seq":2,"status":200}` + "\n" + `not json` + "\n" + `{"seq":1,"status":200}` + "\n" + `{"seq":3,"sta`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if got := seqs(log.Turns()); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("seqs = %v, want [1 2]", got)
	}
	if n := lineCount(t, path); n != 2 {
		t.Fatalf("%d lines after cleanup, want 2", n)
	}
	log.Append(feed.Turn{Seq: 3})
	if n := lineCount(t, path); n != 3 {
		t.Fatalf("%d lines after append, want 3", n)
	}
}

func TestRefusesSharedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not enforced on Windows")
	}
	path := filepath.Join(t.TempDir(), "turns.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, 10); err == nil || !strings.Contains(err.Error(), "accessible to other users") {
		t.Fatalf("err = %v", err)
	}
}
