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
- **Next — remote control:** replace the Claude mobile app's Remote Control
  functions, in order: (1) per-turn model / token / cost records; (2) a live
  event feed plus a mobile web view; (3) authentication and remote access
  (e.g. over Tailscale); (4) tool-permission approval via Claude Code hooks;
  (5) prompt injection via the `Stop` hook. Traffic alone only observes a
  session; steering it needs hooks.
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
- **Brotli:** `github.com/andybalholm/brotli` (pure Go) decodes `br`
  response bodies for `--log-responses` only; the relayed bytes are never
  touched. Justification: the Anthropic API answers `Accept-Encoding: br`
  (sent by Node `fetch`) with Brotli, the stdlib has no decoder, and
  rewriting `Accept-Encoding` would break transparency.
- **Module path:** `github.com/joncbenderkh/cc-proxy`.

## Layout

```
main.go                  cobra root command, flag validation, server lifecycle
VERSION                  single source of truth for the version
internal/proxy/          transparent reverse proxy + per-exchange logging
internal/sse/            server-sent event stream parsing
internal/usage/          model / token / cost extraction and the price table
internal/transcript/     prompt / reply text and tool calls of a turn
internal/feed/           in-memory turn hub, SSE /events, embedded web page
internal/auth/           UI login token, cookie login, request guard
internal/approval/       remote answers to PermissionRequest hooks
internal/prompt/         remote prompts for idle sessions (Stop hooks)
internal/push/           Web Push: VAPID key, subscriptions, encryption
hook.go                  `cc-proxy hook stop`, the Stop hook command
notify.go                which approvals and idle sessions become pushes
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
go run . --log-responses                                   # also log response headers + bodies
go run . --pretty                                          # indented JSON log records
go run . --ui-listen 127.0.0.1:8788                        # live feed web page
go run . hook stop --ui-url http://127.0.0.1:8788          # Stop hook (reads hook input on stdin)
```

Every successful `POST /v1/messages` exchange record carries a `message`
object (`id`, `model`, `stop_reason`, `usage`, `cost_usd`), or a
`message_error` when the response could not be read; `cost_usd` is omitted
for models without a known price. Records also carry `session_id` from the
`X-Claude-Code-Session-Id` request header when present.

`--ui-listen` serves a mobile web page (`/`) and an SSE stream of turns
(`/events`, resumable via `Last-Event-ID`) from an in-memory ring of the
last 500 turns. The page opens on a list of sessions (project from the
system prompt's working directory, title from the first prompt, status,
activity and cost), sorted with those needing an answer first; a session
opens at `#s=<session_id>`. Both require the token in `--ui-token-file` (default
`<user config dir>/cc-proxy/ui-token`, created 0600 on first run; delete it
to rotate): open `/login?token=…` or paste it into the login form once to
get a 400-day HttpOnly cookie, or send `Authorization: Bearer …`. The token
is never logged, only its file path. The UI is plain HTTP, so it only
accepts loopback addresses; reach it from a phone with `tailscale serve`,
which adds TLS.

The UI server also answers Claude Code `PermissionRequest` HTTP hooks at
`POST /hooks/permission-request` (bearer token required). The hook is
held and the prompt appears on the page with Allow / Deny / Always allow
(the suggested `addRules` allow entries only); `POST /approvals/{id}`
records the answer. Claude Code shows its terminal dialog at the same
time, and the first answer wins. Claude Code keeps the hook request open
after a terminal answer, so the proxy watches each `/v1/messages` request
as it goes upstream: once it carries the tool call's result, the prompt is
withdrawn from the page and the hook gets an empty 200 (matched by
`tool_use_id` when the hook input has one, else by tool name and input).
On shutdown the hook also returns an empty 200. Approval log
records carry the tool name and outcome, never the tool input. Hook setup
in `~/.claude/settings.json`, with `CC_PROXY_UI_TOKEN` exported from the
token file:

```json
{"hooks": {"PermissionRequest": [{"hooks": [{
  "type": "http",
  "url": "http://127.0.0.1:8788/hooks/permission-request",
  "timeout": 600,
  "headers": {"Authorization": "Bearer $CC_PROXY_UI_TOKEN"},
  "allowedEnvVars": ["CC_PROXY_UI_TOKEN"]
}]}]}}
```

Prompts from the page reach an idle session through `cc-proxy hook stop`,
run as an `asyncRewake` command hook on `Stop`: it runs in the background,
so the terminal stays usable, and posts the hook input to
`POST /hooks/stop`, which holds until a viewer sends a prompt with
`POST /prompts/{session_id}`. The command then writes the prompt to stderr
and exits 2, which wakes Claude with it as a system reminder; it exits 0
when the session stops again (the newer wait replaces the older one) or
the server shuts down. The page shows a prompt box for the selected
session while it waits. Prompt log records carry the prompt size, never
its text. The command reads the token file itself, so no environment
variable is needed:

```json
{"hooks": {"Stop": [{"hooks": [{
  "type": "command",
  "command": "/path/to/cc-proxy",
  "args": ["hook", "stop", "--ui-url", "http://127.0.0.1:8788"],
  "asyncRewake": true,
  "timeout": 86400
}]}]}}
```

Push notifications reach a phone without the page open. "Notify me" on
the page subscribes the browser (standard Web Push, no third-party
service beyond the browser's own push service); it needs a secure
context, so use the `tailscale serve` URL, and on iOS 16.4+ the page must
first be added to the Home Screen. A new permission prompt pushes at
once; a session that has waited 60 s for its next prompt pushes with the
start of Claude's last reply. Tapping a notification opens that session.
The VAPID key (`vapid-key`) and the subscriptions
(`push-subscriptions.json`) live 0600 in `<user config dir>/cc-proxy/`;
the push package is stdlib only (RFC 8291 aes128gcm, RFC 8292 ES256
tokens). `/sw.js`, `/manifest.webmanifest` and `/icon.png` are served
without login, since browsers fetch them without cookies.

Log records go to stdout as JSON lines; only error-level records (and CLI
errors) go to stderr, so `cc-proxy > claude.log` captures the traffic log.

Releases: bump `VERSION`, merge, then tag `vX.Y.Z` on `main` and push the
tag; `release.yml` verifies the tag matches `VERSION`, cross-compiles
linux/darwin/windows x amd64/arm64, and publishes archives + checksums.

## Conventions

- Keep usage/cost extraction pure and table-tested against recorded API
  responses (both JSON and SSE); confine network I/O to the proxy handler.
- Pricing data lives in one place (`internal/usage/pricing.go`) and carries
  the date it was last checked (`PricesCheckedOn`).

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
