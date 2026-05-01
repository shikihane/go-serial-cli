---
name: serial-cli
description: Use when Codex needs to operate serial devices with the sio Go CLI, manage named serial sessions, inspect ports, send or read serial data, check cached output incrementally, tee logs, expose TCP bridges, share virtual COM ports, pause or resume flashing workflows, install this skill, or debug sio state on Windows.
---

# Serial CLI

Use `sio` as a single-binary CLI for agent-friendly serial-port work. Optimize for Windows behavior first. Keep user-facing commands short (`sio open`, `sio send`, `sio tcp`), and do not invent daemon/backend concepts in normal workflows.

If `sio` is not installed but `gs` is, use `gs` as the legacy alias.

## Default Workflow

1. Discover ports: `sio ports`.
2. Name and start the device worker: `sio open dev1 COM3 -b 115200`.
3. Send bytes through that worker: `sio send dev1 "AT\r\n"` or `sio send dev1 "\x03"`.
4. For request/response devices, ask once: `sio ask dev1 "AT\r\n"`; add `-t 1.5 -l 5` when the response needs more time or fewer lines.
5. Check liveness before expecting new output: `sio status dev1`.
6. Read cached output without consuming it: `sio read dev1 -n 200`.
7. Poll only new cached output: `sio check dev1 -n 200`.
8. Reset output state when needed: `sio clear dev1`.
9. Use one foreground or bridge mode only when needed:
   - `sio shell dev1` for foreground interactive access to the running session.
   - `sio tee dev1 serial.log` for foreground logging to terminal, file, and cache.
   - `sio tcp dev1 :7001` for a background TCP bridge.
   - `sio share dev1 COM20 COM21` for com0com virtual-port sharing.
10. Inspect before cleanup: `sio status dev1` and `sio list`.
11. Clean only the named session: `sio stop dev1`; use `sio rm dev1` to also delete state, cache, logs, and extracted tools.

## Command Reference

```bash
sio version
sio -v
sio ports
sio open dev1 COM3 -b 115200
sio send dev1 "AT\r\n"
sio send dev1 "\x03"
sio ask dev1 "AT\r\n"
sio ask dev1 "ATI\r\n" -t 1.5 -l 5
sio read dev1 -n 200
sio read dev1 --to serial-cache.log
sio check dev1 -n 200
sio check dev1 --rewind 2000
sio check dev1 --from 0 --to checked.log
sio clear dev1
sio shell dev1
sio tee dev1 serial.log
sio tcp dev1 :7001
sio share dev1 COM20 COM21
sio pause dev1
sio resume dev1
sio status dev1
sio log dev1
sio stop dev1
sio rm dev1
sio list
sio skill install .
```

## Payload Escapes

Use explicit byte escapes when sending control characters or line endings. `sio send` and entered `sio shell` lines support:

| Escape | Meaning |
| --- | --- |
| `\r` | carriage return |
| `\n` | line feed |
| `\t` | tab |
| `\xNN` | one hexadecimal byte |
| `\cX` | ASCII control character `Ctrl+X` |

```bash
sio send dev1 "\x03"  # Ctrl+C
sio send dev1 "\cC"   # Ctrl+C
sio send dev1 "\x1b"  # ESC
sio send dev1 "\x04"  # Ctrl+D
```

Do not treat bare `^C` as special. It is ordinary payload text. In `sio shell`, use escaped line endings such as `AT\r\n` when the device expects CRLF.

`sio ask <session> <data>` sends one payload and immediately reads fresh response data. By default it reads for 0.5 seconds and prints the last 50 response lines. Use `-t <seconds>` to change the response window, `-l <lines>` to print the last N lines, and `-l 0` to disable the line limit. When a session worker is running, `sio ask` sends through that worker and reads only newly cached output after the send.

## Cache Semantics

`sio read` is a non-destructive cache viewer. It never advances a cursor, consumes bytes, or truncates the cache. Prefer `--to <file>` for large output so the CLI streams data into a file instead of dumping it to the terminal; combine `-n` with `--to` to export only the last N bytes.

`sio check` is incremental polling. It reads from the saved check cursor and advances that cursor only to the bytes emitted. Use `--rewind <bytes>` to back up from the saved cursor, or `--from <offset>` to inspect from an absolute cache offset. `sio clear <session>` clears `cache.log` and resets the check cursor.

`sio open` starts a session worker that owns the physical serial port and appends output to the cache until `sio stop` or `sio rm`. Other owners with readable serial streams, including `sio tee`, `sio tcp`, and sharing workers, also append output to the cache while they own the stream.

Run `sio status <session>` before expecting new output. `stopped` and `stale` sessions are not live serial readers, so `sio read` and `sio check` can only show bytes already present in `cache.log`. Do not keep polling `sio read` or `sio check` expecting new device output from a stopped or stale session.

If the session is `stopped`, run `sio open <session> <port> -b <baud>` to reopen it and restart the background reader. If the session is `stale`, clean it with `sio stop <session>`, then reopen it with `sio open`.

## Session State

Named sessions let multiple agents or devices coexist. Always include the session name on mutating commands and never clean up all sessions for a named-session request.

`sio open <session> <port>` records or updates local session state, starts a background worker, and keeps the physical port open for that named session. `sio send`, `sio ask`, `sio shell`, and `sio read` then coordinate through that session instead of competing for the physical COM port.

Session files live under the user config directory:

```text
%AppData%/gs/sessions/<session>/
```

The `gs` directory name is retained for compatibility; do not infer that the
canonical command is `gs`.

Important files:

```text
state.json
cache.log
worker.log
```

Use `sio log dev1` to print `worker.log` for session and share diagnosis.

`sio status dev1` reports worker liveness as:

| State | Meaning |
| --- | --- |
| `running` | saved PID appears alive |
| `stale` | saved PID no longer appears alive |
| `stopped` | no saved PID |

`stale` is not fatal. `sio stop dev1` should still clean that session's saved resources and should not fail merely because a saved process is gone. `sio rm dev1` performs the same live cleanup, then removes the session directory.

When `worker_state` is `stopped`, nothing is appending serial output in the background. Reading the cache is still valid for old logs, but it is not a live device read. If the task needs fresh device output, reopen the session first, for example `sio open dev1 COM3 -b 115200`, before checking the cache again.

## Long-Running Modes

Use `sio shell dev1` when an agent needs foreground interactive access. It connects to the running session, prints serial output, and writes stdin lines to the port. Exiting shell leaves the background session worker running. One Ctrl+C should send byte `0x03` to the device; a second interrupt shortly after exits the shell.

Use `sio tee dev1 serial.log` when the main goal is recording device output. It writes to terminal, the requested file, and the session cache.

Use `sio tcp dev1 :7001` when another process should connect over TCP. If the listen argument has no port, the CLI may normalize it to the default TCP listen port. `sio status dev1` shows the saved TCP address and worker log path.

Use `sio share dev1 COM20 COM21` only when com0com virtual-port sharing is needed. `sio stop dev1` must remove only that session's virtual port pairs.

Use `sio pause dev1` before burn or flash workflows that need exclusive access. `sio send`, `sio shell`, and other active serial commands should reject paused sessions until `sio resume dev1`.

## Windows and Tools

Core serial behavior must go through `go.bug.st/serial` via `sio`; do not use PowerShell, WMI, or ad-hoc OS parsing for production serial operations. PowerShell is acceptable only for temporary diagnostics.

For `sio share`, require com0com to be installed explicitly and `setupc.exe` to be discoverable on `PATH` or in a standard Program Files location. Do not silently install drivers. If com0com is missing, fail with an actionable setup message.

`sio` does not bundle com0com installers. The share worker uses the Go bridge built into `sio`.

## Skill Installation

Install this skill from the repository root. Default install targets both Codex and Claude:

```bash
sio skill install .
sio skill install . --to codex
sio skill install . --to claude
sio skill install . --to ./.tmp-skills
```

The default target is both `~/.codex/skills/serial-cli` and `~/.claude/skills/serial-cli`. Custom `--to <dir>` installs under `<dir>/serial-cli`. Remove `.tmp-skills` after manual install checks.

Do not add remote registries, version solving, GitHub installs, package dependencies, or runtime plugin execution to skill installation yet. The first skill model is file installation for agent context.

## Troubleshooting

| Symptom | First checks |
| --- | --- |
| Port missing | Run `sio ports`; verify Windows Device Manager; avoid assuming COM names. |
| `Access is denied` or busy port | Check `sio status <session>`, stop only the owning named session, and close other terminals/tools. |
| No output from `read` | Run `sio status <session>`; if stopped, run `sio open <session> <port> -b <baud>` first. If stale, run `sio stop <session>` and reopen it. |
| Missed output with `check` | Use `sio check <session> --rewind <bytes>` or `--from <offset>`. |
| Worker startup failed | Read `worker.log`; `sio status` may surface `worker_error`. |
| Share problem | Read `worker.log`; confirm com0com `setupc.exe` is installed and discoverable. |
| Stale PID | Run `sio stop <session>`; cleanup should handle missing processes. |

## Repository Guidance

Keep command parsing in `internal/cli`, serial-port behavior in `internal/serialcmd`, local state and cache paths in `internal/session`, and skill installation in `internal/skill`. Do not add a daemon/backend process until the CLI behavior and command shape are proven.

Prefer external behavior tests under `tests/` (`cli_test`, `session_test`, `skill_test`) for command behavior. Package-local tests are acceptable for unexported stream-copying, tee/cache fan-out, and process helpers that are hard to exercise through the public CLI.

When behavior changes, update tests first when practical, then implementation. Keep Windows behavior solid before optimizing Linux behavior.

Before claiming repository changes are complete, run:

```bash
go test ./...
go build -o bin/sio.exe ./cmd/sio
go build -o bin/gs.exe ./cmd/gs
```

Useful Windows command checks:

```bash
go run ./cmd/sio version
go run ./cmd/sio help
go run ./cmd/sio ports
go run ./cmd/sio skill install . --to ./.tmp-skills
```

Remove `.tmp-skills` after manual install tests. Do not commit `bin/`, `dist/`, or `.tmp-skills/`.
