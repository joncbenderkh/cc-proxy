// SPDX-License-Identifier: GPL-3.0-or-later

// Package history keeps the newest turns of the live feed in a JSON Lines
// file, so the feed survives a restart.
package history

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"

	"github.com/joncbenderkh/cc-proxy/internal/feed"
)

// Log appends one line per turn and rewrites the file to the newest turns
// once it holds twice as many lines as it keeps.
type Log struct {
	path string
	keep int

	mu     sync.Mutex
	file   *os.File
	lines  int
	recent []feed.Turn
}

// Open reads the turns stored at path, keeping the newest keep of them,
// and opens the file for appending. It creates the file, readable only by
// its owner, when it does not exist, and skips lines it cannot read, such
// as one cut short by a crash.
func Open(path string, keep int) (*Log, error) {
	if keep < 1 {
		return nil, fmt.Errorf("history must keep at least one turn, not %d", keep)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := checkPrivate(path); err != nil {
			return nil, err
		}
	}
	l := &Log{path: path, keep: keep}
	valid := 0
	for line := range bytes.Lines(data) {
		l.lines++
		var turn feed.Turn
		if json.Unmarshal(line, &turn) == nil && turn.Seq > 0 {
			l.recent = append(l.recent, turn)
			valid++
		}
	}
	slices.SortFunc(l.recent, func(a, b feed.Turn) int { return cmp.Compare(a.Seq, b.Seq) })
	l.trim()
	if valid < l.lines || l.lines > 2*keep {
		return l, l.rewrite()
	}
	l.file, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	return l, err
}

func checkPrivate(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("history file %s is accessible to other users (mode %v); run chmod 600 on it", path, info.Mode().Perm())
	}
	return nil
}

// Turns returns the stored turns, oldest first.
func (l *Log) Turns() []feed.Turn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.recent)
}

// Append stores turn. It does nothing once the log is closed.
func (l *Log) Append(turn feed.Turn) error {
	line, err := json.Marshal(turn)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	if _, err := l.file.Write(append(line, '\n')); err != nil {
		return err
	}
	l.lines++
	l.recent = append(l.recent, turn)
	l.trim()
	if l.lines > 2*l.keep {
		return l.rewrite()
	}
	return nil
}

// Close closes the file; later turns are not stored.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *Log) trim() {
	if len(l.recent) > l.keep {
		l.recent = l.recent[len(l.recent)-l.keep:]
	}
}

// rewrite replaces the file with the recent turns and reopens it for
// appending. The rename keeps the old file intact until the new one is
// complete.
func (l *Log) rewrite() error {
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	temp, err := os.CreateTemp(filepath.Dir(l.path), filepath.Base(l.path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	var buf bytes.Buffer
	for _, turn := range l.recent {
		line, err := json.Marshal(turn)
		if err != nil {
			temp.Close()
			return err
		}
		buf.Write(append(line, '\n'))
	}
	if _, err := temp.Write(buf.Bytes()); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), l.path); err != nil {
		return err
	}
	l.lines = len(l.recent)
	l.file, err = os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND, 0o600)
	return err
}
