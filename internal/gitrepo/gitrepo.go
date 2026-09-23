// SPDX-License-Identifier: GPL-3.0-or-later

// Package gitrepo identifies the git repository a directory belongs to by
// reading its .git files directly, without running git.
package gitrepo

import (
	"bufio"
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Repo names a checkout by where it came from and what it has checked out.
type Repo struct {
	// Root is the top directory of the working tree.
	Root string
	// Remote is the fetch URL of the branch's upstream remote, else of
	// origin, else of the first remote; empty when there is none. User
	// info is stripped from URLs that carry it.
	Remote string
	// Branch is the checked-out branch, or the abbreviated commit when
	// HEAD is detached.
	Branch string
}

// Find returns the repository containing dir, searching upward. It
// reports false when dir is not inside a git working tree.
func Find(dir string) (Repo, bool) {
	root, gitDir, ok := locate(filepath.Clean(dir))
	if !ok {
		return Repo{}, false
	}
	repo := Repo{Root: root, Branch: branch(gitDir)}
	repo.Remote = remote(readConfig(filepath.Join(commonDir(gitDir), "config")), repo.Branch)
	return repo, true
}

// locate walks up from dir to the first directory holding .git, which is
// either the git directory itself or, in a linked worktree or submodule,
// a file naming it.
func locate(dir string) (root, gitDir string, ok bool) {
	for {
		dotGit := filepath.Join(dir, ".git")
		if info, err := os.Stat(dotGit); err == nil {
			if info.IsDir() {
				return dir, dotGit, true
			}
			if target, err := os.ReadFile(dotGit); err == nil {
				if path, found := strings.CutPrefix(strings.TrimSpace(string(target)), "gitdir:"); found {
					return dir, resolve(dir, strings.TrimSpace(path)), true
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

func resolve(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

// commonDir returns the directory shared by all worktrees, which holds
// the config.
func commonDir(gitDir string) string {
	if path, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		return resolve(gitDir, strings.TrimSpace(string(path)))
	}
	return gitDir
}

func branch(gitDir string) string {
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(head))
	if ref, found := strings.CutPrefix(text, "ref:"); found {
		ref = strings.TrimSpace(ref)
		if name, found := strings.CutPrefix(ref, "refs/heads/"); found {
			return name
		}
		return ref
	}
	if len(text) > 7 {
		return text[:7]
	}
	return text
}

// config maps "section.subsection.key" (section and key lower-cased) to
// its first value.
type config struct {
	values  map[string]string
	remotes []string
}

// readConfig parses the subset of git's config syntax needed here:
// [section "subsection"] headers and key = value lines.
func readConfig(path string) config {
	cfg := config{values: map[string]string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	var section string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			header, _, _ := strings.Cut(line[1:], "]")
			name, sub, quoted := strings.Cut(header, " ")
			section = strings.ToLower(strings.TrimSpace(name))
			if quoted {
				sub = strings.Trim(strings.TrimSpace(sub), `"`)
				section += "." + sub
				if strings.HasPrefix(section, "remote.") {
					cfg.remotes = append(cfg.remotes, sub)
				}
			}
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		name := section + "." + strings.ToLower(strings.TrimSpace(key))
		if _, seen := cfg.values[name]; !seen {
			cfg.values[name] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return cfg
}

func remote(cfg config, branch string) string {
	candidates := []string{cfg.values["branch."+branch+".remote"], "origin"}
	candidates = append(candidates, cfg.remotes...)
	for _, name := range candidates {
		if raw := cfg.values["remote."+name+".url"]; name != "" && raw != "" {
			return stripUserInfo(raw)
		}
	}
	return ""
}

// stripUserInfo drops credentials from scheme URLs such as
// https://user:token@host/repo; scp-like addresses (git@host:repo) are
// returned unchanged.
func stripUserInfo(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme == "ssh" {
		return raw
	}
	u.User = nil
	return u.String()
}
