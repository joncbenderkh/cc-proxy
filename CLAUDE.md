# CLAUDE.md

Guidance for Claude Code (claude.ai/code) when working in this repository.

## What cc-proxy is

`cc-proxy` — a local reverse proxy for Claude Code. Point Claude Code at it with
`ANTHROPIC_BASE_URL=http://localhost:<port>`; it forwards every request to the
Anthropic API unchanged (including SSE streaming responses) and records what
passed through.

- **Now — observability:** log each request/response, extract model, token
  usage (input / output / cache read / cache write), latency, and computed
  cost; make that history inspectable.
- **Later — policy / gateway:** budgets, rate limits, key management, and
  redaction, layered on the same pipeline. Design the request path so these
  can slot in without restructuring, but do not build them yet.

The proxy must stay transparent: never alter request or response bodies in
the observability phase, never buffer a stream before relaying it, and never
log the `x-api-key` / `Authorization` header values.

## Decisions

- **License:** GPL-3.0-or-later. `LICENSE` at repo root;
  `SPDX-License-Identifier: GPL-3.0-or-later` as the first line of every `.go`
  file. `main.go` also carries a `// cc-proxy — …` +
  `// Copyright (C) 2026 Jon Bender` block.
- **Version:** `./VERSION` is the single source of truth, embedded into the
  binary at build time (`//go:embed VERSION` in `main.go`); a release tag
  `vX.Y.Z` mirrors it.
- **Language / stack:** Go (target the current stable release in `go.mod`).
  Single static binary, `CGO_ENABLED=0` always. Standard library first
  (`net/http`, `net/http/httputil`, `encoding/json`, `log/slog`, `embed`);
  justify every non-stdlib dependency, and any storage driver must be pure Go
  so the cross-compile release matrix keeps working.
- **CLI:** `github.com/spf13/cobra` (with `spf13/pflag`). Justification:
  GNU-style `--long` flags, strict rejection of unknown flags, single-dash
  long flags and stray positional arguments, and generated help; all pure
  Go. Every flag value is validated before the server starts.
- **Module path:** `github.com/joncbenderkh/cc-proxy`.

## Layout

```
main.go                  cobra root command, flag validation, server lifecycle
VERSION                  single source of truth for the version
internal/proxy/          transparent reverse proxy + per-exchange logging
.github/workflows/       ci.yml (checks), release.yml (tag -> GitHub Release)
```

`proxy.New` wraps an `httputil.ReverseProxy` (`FlushInterval: -1`) in
`http.Handler` middleware; future usage extraction and policy layers slot
in as further middleware around it. Response-writer wrappers must implement
`Unwrap()` so `http.ResponseController` can still flush streams.

## Commands

```
go build -trimpath ./...                                   # build (CGO_ENABLED=0)
go test ./...                                              # test
go test -race ./...                                        # race test (needs cgo)
gofmt -l .                                                 # format check
go vet ./...                                               # vet
go run honnef.co/go/tools/cmd/staticcheck@2026.2.1 ./...   # lint
go run . --listen 127.0.0.1:8787                           # run locally
go run . --log-requests                                    # also log outbound headers + bodies
go run . --pretty                                          # indented JSON log records
```

Releases: bump `VERSION`, merge, then tag `vX.Y.Z` on `main` and push the
tag; `release.yml` verifies the tag matches `VERSION`, cross-compiles
linux/darwin/windows x amd64/arm64, and publishes archives + checksums.

## Conventions

- Keep usage/cost extraction pure and table-tested against recorded API
  responses (both JSON and SSE); confine network I/O to the proxy handler.
- Pricing data lives in one place and carries the date it was last checked.

## Versioning

SemVer 2.0.0. `./VERSION` is the single source of truth; the release tag
mirrors it (`vX.Y.Z`). During 0.x, a minor bump may carry a breaking change;
patch releases stay compatible.

## Working on cc-proxy

- Never commit to `main`. Start every change on a branch, ideally a worktree
  created with the `gwa <branch> <comment>` alias (siblings of `main/`, per the
  `projects/` layout). Commit there, `git push -u origin <branch>`, open a PR,
  and report the PR URL.
- Conventional Commits: `<type>(<scope>): <subject>`, imperative, <=50 chars, no
  trailing period. Body wrapped at 72 for non-trivial changes. No attribution
  lines.
- English only, throughout.
