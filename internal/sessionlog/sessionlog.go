// SPDX-License-Identifier: GPL-3.0-or-later

// Package sessionlog reads Claude Code's own local transcript of a
// session (<config dir>/projects/<encoded-cwd>/<session-id>.jsonl,
// normally under ~/.claude) so a session's working directory, branch,
// title and account email can be known before any request for it has
// passed through the proxy. This is a read-only, best-effort supplement
// to the traffic the proxy relays; its file format is Claude Code's own
// and undocumented, so a lookup that fails or finds nothing is not an
// error.
package sessionlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/joncbenderkh/cc-proxy/internal/transcript"
)

// Session is what a local transcript can tell us about a session before
// any of its requests have been observed. Fields are empty when the
// transcript doesn't carry them yet.
type Session struct {
	Cwd    string
	Branch string
	Title  string
	User   string
}

// maxScan bounds how many lines are read looking for a session's fields,
// so a very long-running session doesn't make a lookup slow.
const maxScan = 2000

// Dirs are the "projects" directories, one per Claude Code config
// directory, searched in order for a session's transcript. It defaults to
// just ~/.claude/projects; running more than one Claude Code identity
// (each launched with its own CLAUDE_CONFIG_DIR, such as
// CLAUDE_CONFIG_DIR=~/.claude-work) means adding their directories too. A
// package variable so tests can point it elsewhere.
var Dirs = defaultDirs()

func defaultDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".claude", "projects")}
}

// Find looks up the local transcript of sessionID across Dirs and reads
// as much of Session as it can find in its first lines. It reports false
// when no transcript is found or none of these fields appear in it.
// Session ids are unique per launch, so the first directory with a
// matching transcript is authoritative; the rest are not searched.
func Find(sessionID string) (Session, bool) {
	if sessionID == "" {
		return Session{}, false
	}
	for _, dir := range Dirs {
		if dir == "" {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(dir, "*", sessionID+".jsonl"))
		if err != nil || len(matches) == 0 {
			continue
		}
		session := read(matches[0])
		return session, session != Session{}
	}
	return Session{}, false
}

// line is the subset of Claude Code's transcript line shapes this
// package reads: "user" and "assistant" lines carry cwd and gitBranch
// directly; the first real user prompt is a non-meta "user" line;
// the account email is in a "session_context" attachment line.
type line struct {
	Type      string `json:"type"`
	Cwd       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`
	IsMeta    bool   `json:"isMeta"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Attachment struct {
		Type    string `json:"type"`
		Context struct {
			UserEmail string `json:"userEmail"`
		} `json:"context"`
	} `json:"attachment"`
}

func read(path string) Session {
	file, err := os.Open(path)
	if err != nil {
		return Session{}
	}
	defer file.Close()

	var session Session
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanCount := 0; scanner.Scan() && scanCount < maxScan; scanCount++ {
		var l line
		if err := json.Unmarshal(scanner.Bytes(), &l); err != nil {
			continue
		}
		if session.Cwd == "" {
			session.Cwd = l.Cwd
		}
		if session.Branch == "" {
			session.Branch = l.GitBranch
		}
		if session.Title == "" && l.Type == "user" && !l.IsMeta && l.Message.Role == "user" {
			session.Title = transcript.TitleOf(l.Message.Content)
		}
		if session.User == "" && l.Type == "attachment" && l.Attachment.Type == "session_context" {
			session.User = transcript.EmailFromSentence(l.Attachment.Context.UserEmail)
		}
		if session.Cwd != "" && session.Branch != "" && session.Title != "" && session.User != "" {
			break
		}
	}
	return session
}
