---
name: serial-cli
description: Use when Codex needs to operate serial devices with the gs Go CLI, manage named serial sessions, inspect ports, send or read serial data, check cached output incrementally, tee logs, expose TCP bridges, share virtual COM ports, pause or resume flashing workflows, install this skill, or debug gs state on Windows.
---

# Serial CLI

Use `gs` as a single-binary CLI for agent-friendly serial-port work. Optimize for Windows behavior first. Keep user-facing commands short (`gs open`, `gs send`, `gs tcp`), and do not invent daemon/backend concepts in normal workflows.

## Default Workflow

1. Discover ports: `gs ports`.
2. Name the device: `gs open dev1 COM3 -b 115200`.
3. Send bytes: `gs send dev1 "AT\r\n"` or `gs send dev1 "\x03"`.
4. Check liveness before expecting new output: `gs status dev1`.
5. Read cached output without consuming it: `gs read dev1 -n 200`.
6. Poll only new cached output: `gs check dev1 -n 200`.
7. Reset output state when needed: `gs clear dev1`.
8. Use one long-running mode only when needed:
   - `gs shell dev1` for foreground interactive serial ownership.
   - `gs tee dev1 serial.log` for foreground logging to terminal, file, and cache.
   - `gs tcp dev1 :7001` for a background TCP bridge.
   - `gs share dev1 COM20 COM21` for com0com virtual-port sharing.
9. Inspect before cleanup: `gs status dev1` and `gs list`.
10. Clean only the named session: `gs stop dev1`; use `gs rm dev1` to also delete state, cache, logs, and extracted tools.

## Command Reference

```bash
gs version
gs -v
gs ports
gs open dev1 COM3 -b 115200
gs send dev1 "AT\r\n"
gs send dev1 "\x03"
gs read dev1 -n 200
gs read dev1 --to serial-cache.log
gs check dev1 -n 200
gs check dev1 --rewind 2000
gs check dev1 --from 0 --to checked.log
gs clear dev1
gs shell dev1
gs tee dev1 serial.log
gs tcp dev1 :7001
gs share dev1 COM20 COM21
gs pause dev1
gs resume dev1
gs status dev1
gs log dev1
gs log dev1 --hub
gs stop dev1
gs rm dev1
gs list
gs skill install
```

## Payload Escapes

Use explicit byte escapes when sending control characters or line endings. `gs send` and entered `gs shell` lines support:

| Escape | Meaning |
| --- | --- |
| `\r` | carriage return |
| `\n` | line feed |
| `\t` | tab |
| `\xNN` | one hexadecimal byte |
| `\cX` | ASCII control character `Ctrl+X` |

```bash
gs send dev1 "\x03"  # Ctrl+C
gs send dev1 "\cC"   # Ctrl+C
gs send dev1 "\x1b"  # ESC
gs send dev1 "\x04"  # Ctrl+D
```

Do not treat bare `^C` as special. It is ordinary payload text. In `gs shell`, use escaped line endings such as `AT\r\n` when the device expects CRLF.

## Cache Semantics

`gs read` is a non-destructive cache viewer. It never advances a cursor, consumes bytes, or truncates the cache. Prefer `--to <file>` for large output so the CLI streams data into a file instead of dumping it to the terminal; combine `-n` with `--to` to export only the last N bytes.

`gs check` is incremental polling. It reads from the saved check cursor and advances that cursor only to the bytes emitted. Use `--rewind <bytes>` to back up from the saved cursor, or `--from <offset>` to inspect from an absolute cache offset. `gs clear <session>` clears `cache.log` and resets the check cursor.

Background owners that have a readable serial stream append output to the cache. This includes `gs tee`, `gs tcp`, and sharing/session workers when they own the stream.

Run `gs status <session>` before expecting new output. `stopped` and `stale` sessions are not live serial readers, so `gs read` and `gs check` can only show bytes already present in `cache.log`. Do not keep polling `gs read` or `gs check` expecting new device output from a stopped or stale session.

If the session is `stopped`, run `gs open <session> <port> -b <baud>` first to reopen the named session. Then start the actual monitor with a live owner such as `gs tee`, `gs shell`, `gs tcp`, or `gs share`. If the session is `stale`, clean it with `gs stop <session>`, then reopen it with `gs open`.

## Session State

Named sessions let multiple agents or devices coexist. Always include the session name on mutating commands and never clean up all sessions for a named-session request.

`gs open <session> <port>` records or updates local session state. Treat it as setup, not as the user's interactive terminal. Use `gs shell`, `gs tee`, `gs tcp`, or `gs share` for active ownership.

Session files live under the user config directory:

```text
%AppData%/gs/sessions/<session>/
```

Important files:

```text
state.json
cache.log
worker.log
hub4com.log
```

Use `gs log dev1` to print `worker.log`. Use `gs log dev1 --hub` to print `hub4com.log` for share/hub4com diagnosis.

`gs status dev1` reports worker and hub liveness as:

| State | Meaning |
| --- | --- |
| `running` | saved PID appears alive |
| `stale` | saved PID no longer appears alive |
| `stopped` | no saved PID |

`stale` is not fatal. `gs stop dev1` should still clean that session's saved resources and should not fail merely because a saved process is gone. `gs rm dev1` performs the same live cleanup, then removes the session directory.

When `worker_state` and `hub_state` are both `stopped`, nothing is appending serial output in the background. Reading the cache is still valid for old logs, but it is not a live device read. If the task needs fresh device output, reopen the session first, for example `gs open dev1 COM3 -b 115200`, then start `gs tee dev1 serial.log`, `gs shell dev1`, `gs tcp dev1 :7001`, or the appropriate sharing workflow before checking the cache again.

## Long-Running Modes

Use `gs shell dev1` when an agent needs foreground interactive access. It prints serial output and writes stdin lines to the port. One Ctrl+C should send byte `0x03` to the device; a second interrupt shortly after exits the shell.

Use `gs tee dev1 serial.log` when the main goal is recording device output. It writes to terminal, the requested file, and the session cache.

Use `gs tcp dev1 :7001` when another process should connect over TCP. If the listen argument has no port, the CLI may normalize it to the default TCP listen port. `gs status dev1` shows the saved TCP address and worker log path.

Use `gs share dev1 COM20 COM21` only when com0com/hub4com virtual-port sharing is needed. `gs stop dev1` must remove only that session's virtual port pairs.

Use `gs pause dev1` before burn or flash workflows that need exclusive access. `gs send`, `gs shell`, and other active serial commands should reject paused sessions until `gs resume dev1`.

## Windows and Tools

Core serial behavior must go through `go.bug.st/serial` via `gs`; do not use PowerShell, WMI, or ad-hoc OS parsing for production serial operations. PowerShell is acceptable only for temporary diagnostics.

For `gs share`, require com0com to be installed explicitly and `setupc.exe` to be discoverable on `PATH` or in a standard Program Files location. Do not silently install drivers. If com0com is missing, fail with an actionable setup message.

Bundled helper installers may be extracted with:

```bash
gs tools extract <dir>
```

Driver installation remains a manual user action after extraction.

## Skill Installation

Install the bundled serial-cli skill after installing `gs`. This does not depend on the current working directory. Default install targets both Codex and Claude:

```bash
gs skill install
gs skill install --to codex
gs skill install --to claude
gs skill install --to ./.tmp-skills
```

The default target is both `~/.codex/skills/serial-cli` and `~/.claude/skills/serial-cli`. Custom `--to <dir>` installs under `<dir>/serial-cli`. Developers may still pass an explicit source directory, such as `gs skill install . --to ./.tmp-skills`, when testing a local skill checkout. Remove `.tmp-skills` after manual install checks.

Do not add remote registries, version solving, GitHub installs, package dependencies, or runtime plugin execution to skill installation yet. The first skill model is file installation for agent context.

## Troubleshooting

| Symptom | First checks |
| --- | --- |
| Port missing | Run `gs ports`; verify Windows Device Manager; avoid assuming COM names. |
| `Access is denied` or busy port | Check `gs status <session>`, stop only the owning named session, and close other terminals/tools. |
| No output from `read` | Run `gs status <session>`; if stopped, run `gs open <session> <port> -b <baud>` first, then start `gs tee`, `gs shell`, `gs tcp`, or `gs share` before expecting new output. |
| Missed output with `check` | Use `gs check <session> --rewind <bytes>` or `--from <offset>`. |
| Worker startup failed | Read `worker.log`; `gs status` may surface `worker_error`. |
| hub4com/share problem | Read `hub4com.log`; confirm com0com `setupc.exe` is installed and discoverable. |
| Stale PID | Run `gs stop <session>`; cleanup should handle missing processes. |

## Repository Guidance

Keep command parsing in `internal/cli`, serial-port behavior in `internal/serialcmd`, local state and cache paths in `internal/session`, and skill installation in `internal/skill`. Do not add a daemon/backend process until the CLI behavior and command shape are proven.

Prefer external behavior tests under `tests/` (`cli_test`, `session_test`, `skill_test`) for command behavior. Package-local tests are acceptable for unexported stream-copying, tee/cache fan-out, and process helpers that are hard to exercise through the public CLI.

When behavior changes, update tests first when practical, then implementation. Keep Windows behavior solid before optimizing Linux behavior.

Before claiming repository changes are complete, run:

```bash
go test ./...
go build -o bin/gs.exe ./cmd/gs
```

Useful Windows command checks:

```bash
go run ./cmd/gs version
go run ./cmd/gs help
go run ./cmd/gs ports
go run ./cmd/gs skill install --to ./.tmp-skills
```

Remove `.tmp-skills` after manual install tests. Do not commit `bin/`, `dist/`, or `.tmp-skills/`.
