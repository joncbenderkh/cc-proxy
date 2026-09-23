// SPDX-License-Identifier: GPL-3.0-or-later

package gitrepo

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFind(t *testing.T) {
	const originConfig = "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = git@github.com:u/repo.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	tests := []struct {
		name   string
		files  map[string]string
		dir    string
		want   Repo
		wantOK bool
	}{
		{
			name:   "main checkout from a subdirectory",
			files:  map[string]string{"main/.git/HEAD": "ref: refs/heads/main\n", "main/.git/config": originConfig, "main/src/x": ""},
			dir:    "main/src",
			want:   Repo{Root: "main", Remote: "git@github.com:u/repo.git", Branch: "main"},
			wantOK: true,
		},
		{
			name: "linked worktree",
			files: map[string]string{
				"main/.git/config":                   originConfig,
				"main/.git/worktrees/feat/HEAD":      "ref: refs/heads/feat/x\n",
				"main/.git/worktrees/feat/commondir": "../..\n",
				"feat/.git":                          "gitdir: ../main/.git/worktrees/feat\n",
			},
			dir:    "feat",
			want:   Repo{Root: "feat", Remote: "git@github.com:u/repo.git", Branch: "feat/x"},
			wantOK: true,
		},
		{
			name: "upstream remote wins over origin",
			files: map[string]string{
				"r/.git/HEAD":   "ref: refs/heads/dev\n",
				"r/.git/config": originConfig + "[remote \"fork\"]\n\turl = https://user:secret@example.com/fork.git\n[branch \"dev\"]\n\tremote = fork\n",
			},
			dir:    "r",
			want:   Repo{Root: "r", Remote: "https://example.com/fork.git", Branch: "dev"},
			wantOK: true,
		},
		{
			name:   "detached head without remotes",
			files:  map[string]string{"r/.git/HEAD": "0123456789abcdef0123456789abcdef01234567\n", "r/.git/config": "[core]\n"},
			dir:    "r",
			want:   Repo{Root: "r", Branch: "0123456"},
			wantOK: true,
		},
		{
			name:  "not a repository",
			files: map[string]string{"plain/x": ""},
			dir:   "plain",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			for path, content := range tt.files {
				write(t, filepath.Join(base, path), content)
			}
			got, ok := Find(filepath.Join(base, tt.dir))
			if tt.wantOK {
				tt.want.Root = filepath.Join(base, tt.want.Root)
			}
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("Find() = %+v, %v; want %+v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
