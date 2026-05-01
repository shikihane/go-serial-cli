# AGENTS.md

This repository is a Go CLI project for serial-port workflows. The primary platform is Windows; Linux support should remain possible, but do not optimize for it before Windows behavior is solid.

## Product Direction

Build `gs` as a simple single-binary CLI first. Do not introduce a daemon/backend process until the CLI behavior and command shape are proven.

The user-facing command style should stay short:

```bash
gs version
gs -v
gs ports
gs open dev1 COM3 -b 115200
gs share dev1 COM20 COM21
gs send dev1 "AT\r\n"
gs send dev1 "\x03"
gs ask dev1 "AT\r\n"
gs ask dev1 "ATI\r\n" -t 1.5 -l 5
gs read dev1 -n 200
gs read dev1 --to serial-cache.log
gs check dev1 -n 200
gs check dev1 --rewind 2000
gs clear dev1
gs shell dev1
gs tee dev1 serial.log
gs tcp dev1 :7001
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

Avoid exposing internal architecture words in normal commands. Prefer `gs open dev1 COM3` over `gs session open dev1 COM3`, and `gs tcp dev1 :7001` over `gs forward start`.

## Skill Install Direction

Keep skill installation minimal:

```bash
gs skill install
gs skill install --to codex
gs skill install --to claude
gs skill install --to <dir>
```

Default install should target both:

```text
~/.codex/skills/serial-cli
~/.claude/skills/serial-cli
```

Do not add remote skill registries, version solving, GitHub installs, package dependencies, or plugin execution yet. The first skill model is file installation for agent context, not a runtime plugin system.

## Architecture

Current layout:

```text
cmd/gs/                 CLI executable entrypoint
internal/cli/           command parsing and command behavior
internal/serialcmd/     serial-port adapter
internal/session/       local session state and cache paths
internal/skill/         skill installation
tests/                  unit tests, grouped by feature
```

Keep command parsing in `internal/cli`. Keep OS and serial-port details out of CLI code where practical.

Use `go.bug.st/serial` for serial behavior. Do not shell out to PowerShell for core serial operations. PowerShell is acceptable only as a temporary diagnostic tool, not production implementation.

## Current Behavior

`gs open <session> <port>` stores a named serial session in the user config directory, starts a background session worker, and keeps the physical port open until `gs stop <session>` or `gs rm <session>`.

`gs ports` enumerates ports through `go.bug.st/serial`.

`gs send <session>` writes one payload through the named session worker when it is running. If no worker/control channel is available, it may open the named session port for that one write and close it. Payloads support explicit escapes: `\r`, `\n`, `\t`, `\xNN` for one hexadecimal byte, and `\cX` for ASCII control characters. For example, `gs send dev1 "\x03"` and `gs send dev1 "\cC"` both send Ctrl+C, `gs send dev1 "\x1b"` sends ESC, and `gs send dev1 "\x04"` sends Ctrl+D. Do not treat bare `^C` as special; it should remain ordinary payload text.

`gs ask <session> <data>` sends one payload and immediately reads fresh response data for request/response devices. By default it reads for 0.5 seconds and prints the last 50 response lines. Use `-t <seconds>` to change the response window, `-l <lines>` to print the last N lines, and `-l 0` to disable the line limit. When a session worker is running, `gs ask` sends through that worker and reads only newly appended cache output after the send. Without a worker/control channel, it may open the named session port for the ask and append the response to the local cache.

`gs read <session>` reads from that session's local cache file without consuming, truncating, or advancing any cursor. Background workers and `tee` append serial output to that cache when they own a readable serial stream. Large cache reads should use `gs read <session> --to <file>` so data is streamed into a file instead of dumped to the terminal; `-n` may be combined with `--to` to export only the last N bytes.

`gs check <session>` is the incremental-read command. It reads from the saved check cursor and advances that cursor only to the bytes it emitted. Use `gs check <session> --rewind <bytes>` to back up if important output was missed, or `gs check <session> --from <offset>` to inspect from an absolute cache offset. `gs clear <session>` resets both cache contents and the check cursor.

`gs shell <session>` connects to the named session worker in the foreground when it is running. It continuously prints serial output and writes stdin lines to the port. Exiting shell must leave the background session worker running. Use escaped line endings such as `AT\r\n` when the device expects CRLF; the same payload escapes as `gs send` apply to entered lines.

`gs tee <session> <file>` opens the named session port and keeps it open in the foreground. It continuously writes serial output to both the screen and the file, and also appends output to the local cache file used by `gs read <session>`.

`gs share <session> <virtual-port>...` creates the com0com virtual port pairs when `setupc.exe` is available, records the virtual ports, and starts a background `gs worker` that launches and supervises hub4com for that session. Driver installation must remain explicit; if com0com is not installed or `setupc.exe` is not on PATH, the command should fail with an actionable error rather than silently installing a driver.

`gs tcp <session> <listen-address>` records a TCP listen address and starts a background `gs worker` that accepts TCP clients and bridges them to the named serial session.

Background workers append lifecycle and retry diagnostics to each session's `worker.log`. hub4com stdout/stderr is written to `hub4com.log`. These files live next to `state.json` and `cache.log` under the session directory.

`gs log <session>` prints that session's `worker.log`. Use `gs log <session> --hub` to print `hub4com.log` when diagnosing share/hub4com behavior.

`gs status <session>` should expose PID liveness as `running`, `stale`, or `stopped`. `stale` means the PID is saved in state but no matching process is running; `gs stop <session>` should still clean only that session's resources without failing on the missing process.

`gs pause <session>` and `gs resume <session>` update local session state. They are preparation for burn/flash workflows and future long-running session ownership.

`gs stop <session>` stops only the named session's worker/hub processes, removes only that session's virtual port pairs, and clears live resource state. It must not stop other sessions because multiple agents may be controlling different devices.

`gs rm <session>` performs the same named-session live cleanup as `gs stop <session>`, then deletes that session directory including `state.json`, `cache.log`, `worker.log`, `hub4com.log`, and extracted tools. It must not remove other sessions.

## Development Rules

Write tests for behavior changes before implementation when practical. At minimum, add or update tests for command parsing, session state, serial stream behavior, and skill installation behavior.

Prefer external behavior tests under `tests/` using packages such as `cli_test`, `session_test`, and `skill_test`. Package-local tests next to implementation are acceptable for small unexported helpers that are hard to exercise without real serial hardware, such as stream copying and tee/cache file fan-out.

Run these before claiming work is complete:

```bash
go test ./...
go build -o bin/gs.exe ./cmd/gs
```

For local installation during development:

```powershell
$commit = git rev-parse --short HEAD
if (git status --porcelain) { $commit = "$commit-dirty" }
$builtAt = Get-Date -Format "yyyy-MM-ddTHH:mm:sszzz"
go install -ldflags "-X go-serial-cli/internal/cli.BuildVersion=dev -X go-serial-cli/internal/cli.BuildCommit=$commit -X go-serial-cli/internal/cli.BuildBuiltAt=$builtAt" ./cmd/gs
```

On Windows, when `GOBIN` is unset, Go installs `gs.exe` to:

```text
%GOPATH%\bin\gs.exe
```

For the current local user, this normally resolves to:

```text
C:\Users\<you>\go\bin\gs.exe
```

Ensure `%GOPATH%\bin` is on `PATH` before expecting `gs` to be available from any terminal.

When asking a human to manually test a CLI behavior change, install the binary first. Building
`bin/gs.exe` is not enough because terminals normally run the `gs.exe` found on `PATH`.
After installing, always verify which binary will run before handing off manual testing.

After installing, verify the active binary with:

```powershell
Get-Command gs
gs version
```

For command checks on Windows:

```bash
go run ./cmd/gs version
go run ./cmd/gs help
go run ./cmd/gs ports
go run ./cmd/gs skill install --to ./.tmp-skills
```

Remove `.tmp-skills` after manual install tests.

## Dependency Policy

Prefer small, established Go libraries. Avoid large frameworks for CLI parsing unless the command surface grows enough to justify them.

Current serial dependency:

```text
go.bug.st/serial
```

Do not replace it with PowerShell, WMI scripts, or ad-hoc OS command parsing for normal serial operations.

## File Hygiene

Do not commit build artifacts or temporary install outputs:

```text
bin/
dist/
.tmp-skills/
```

Keep files ASCII unless there is a specific reason otherwise.
