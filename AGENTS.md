# AGENTS.md

This repository is a Go CLI project for serial-port workflows. The primary platform is Windows; Linux support should remain possible, but do not optimize for it before Windows behavior is solid.

## Product Direction

Build `sio` as a simple single-binary CLI first. Do not introduce a daemon/backend process until the CLI behavior and command shape are proven.

The user-facing command style should stay short:

```bash
sio version
sio -v
sio ports
sio open dev1 COM3 -b 115200
sio open rawdev COM4 --raw
sio send dev1 AT
sio send dev1 --raw "\x03"
sio ask dev1 ATI
sio ask dev1 ATI -t 1.5 -l 5
sio share dev1 COM20 COM21
sio read dev1 -n 200
sio read dev1 --to serial-cache.log
sio read dev1 --all -n 50
sio read dev1 --tx
sio check dev1 -n 200
sio check dev1 --rewind 2000
sio clear dev1
sio shell dev1
sio sz dev1 note.bin
sio sz dev1 a.wav b.wav --conflict overwrite
sio rz dev1
sio rz dev1 note.bin
sio rz dev1 downloads/
sio shell dev1 --no-auto-transfer
sio tee dev1 serial.log
sio tcp dev1 :7001
sio pause dev1
sio resume dev1
sio status dev1
sio log dev1
sio stop dev1
sio rm dev1
sio clear --share
sio list
sio skill install
```

Avoid exposing internal architecture words in normal commands. Prefer `sio open dev1 COM3` over `sio session open dev1 COM3`, and `sio tcp dev1 :7001` over `sio forward start`.

## Skill Install Direction

Keep skill installation minimal:

```bash
sio skill install
sio skill install --to codex
sio skill install --to claude
sio skill install --to <dir>
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
cmd/sio/                 CLI executable entrypoint
cmd/gs/                  compatibility alias entrypoint
internal/cli/           command parsing and command behavior
internal/serialcmd/     serial-port adapter
internal/session/       local session state and cache paths
internal/skill/         skill installation
tests/                  unit tests, grouped by feature
```

Keep command parsing in `internal/cli`. Keep OS and serial-port details out of CLI code where practical.

Use `go.bug.st/serial` for serial behavior. Do not shell out to PowerShell for core serial operations. PowerShell is acceptable only as a temporary diagnostic tool, not production implementation.

## Current Behavior

`sio open <session> <port>` stores a named serial session in the user config directory, starts a background session worker, and keeps the physical port open until `sio stop <session>` or `sio rm <session>`. Text `send` and `ask` use line mode by default for normal command devices. Use `sio open <session> <port> --raw` when that session should send raw text payloads by default.

`sio ports` enumerates ports through `go.bug.st/serial`.

`sio send <session>` writes one payload through the named session worker when it is running. If no worker/control channel is available, it may open the named session port for that one write and close it. Text payloads append `\r\n` by default unless the text already contains an explicit `\r` or `\n`; use `sio send <session> --raw <data>` for a one-shot raw text payload. Payloads support explicit escapes: `\r`, `\n`, `\t`, `\xNN` for one hexadecimal byte, and `\cX` for ASCII control characters. For example, `sio send dev1 --raw "\x03"` and `sio send dev1 --raw "\cC"` both send Ctrl+C, `sio send dev1 --raw "\x1b"` sends ESC, and `sio send dev1 --raw "\x04"` sends Ctrl+D. Do not treat bare `^C` as special; it should remain ordinary payload text. Hex and file payloads are always raw.

`sio ask <session> <data>` sends one payload and immediately reads fresh response data for request/response devices. Text payloads use the same default CRLF line mode as `send`; use `--raw` for a one-shot raw text request. By default it reads for 0.5 seconds and prints the last 50 response lines. Use `-t <seconds>` to change the response window, `-l <lines>` to print the last N lines, and `-l 0` to disable the line limit. When a session worker is running, `sio ask` sends through that worker and reads only newly appended cache output after the send. Without a worker/control channel, it may open the named session port for the ask and append the response to the local cache.

`sio read <session>` reads from that session's local cache file without consuming, truncating, or advancing any cursor. Background workers and `tee` append serial output to that cache when they own a readable serial stream. Large cache reads should use `sio read <session> --to <file>` so data is streamed into a file instead of dumped to the terminal; `-n` may be combined with `--to` to export only the last N bytes.

`sio check <session>` is the incremental-read command. It reads from the saved check cursor and advances that cursor only to the bytes it emitted. Use `sio check <session> --rewind <bytes>` to back up if important output was missed, or `sio check <session> --from <offset>` to inspect from an absolute cache offset. `sio clear <session>` resets cache contents (`cache.log` and `tx.log`) and the check cursor.

`sio read <session> --all` prints a merged, timestamped TX/RX timeline; `--tx` shows captured TX only. Every session worker (open, tcp, share) captures every sender to `tx.log` with a source tag: external programs writing through shared virtual ports are tagged by public port name (`TX[COM20]`), and control-channel sends from `sio send`/`ask`/`shell` are tagged `TX[tcp:<addr>]`. With `--all`/`--tx`, `-n` means last N lines and `-x` switches to hex; text mode escapes non-printable bytes as `\xNN` for display while `tx.log` keeps raw bytes. These views are non-destructive and never advance the check cursor; plain `sio read` output stays raw RX bytes. `sio shell` also overlays other senders' live TX as tagged lines (`[COM20→] ...`), assembling partial writes, escaping non-printables, truncating payloads over 64 bytes, and filtering the shell's own sends.

`sio sz <session> <file>...` sends one or more local files with ZMODEM, and `sio rz <session> [dest]` receives every file the device sends; `dest` is a folder, or the path of the first file. The protocol follows Forsberg's 1988 ZMODEM specification and must interoperate with lrzsz and other standard tools: CRC32 when the peer offers it, ZDLE escaping of ZDLE/DLE/XON/XOFF (all control characters when either side asks with `ESCCTL`/`TESCCTL`), a sliding window `W = min(R, Tw)` where `R` is the receiver's `ZRINIT` buffer (0 means unlimited) and `Tw` is `--window`, `ZCRCQ` checkpoints every `W/4` bytes answered by `ZACK`, `ZRPOS` resync that halves the subpacket length and grows it back after clean subpackets, and `ZCRCW` stop-and-wait only for a receiver without `CANFDX`. sio sends subpackets of up to 1024 bytes and accepts up to 8192. A receiver writes `<name>.zpart` next to the target and renames it when the file completes; a failed or cancelled file keeps the `.zpart` as the resume point. The receiver sends `ZCRC` only when the offer carries `ZCRESUM` or `ZMCRC`: with `ZCRESUM` the next transfer resumes after `ZCRC` confirms the prefix, and without an answer it starts from 0 (never an unchecked resume); without `ZCRESUM` an existing `.zpart` is restarted from 0, and without either an existing target under the default policy is renamed rather than compared. Waits for an answer to `ZFILE` or `ZCRC` are the handshake wait (at least 120 s) plus 1 s per 64 KB of file, since the peer may be computing a CRC on a slow card. `--conflict` picks `skip-identical-else-rename`, `overwrite`, `skip`, or `rename`. An explicit `--conflict` wins over the management option the sender puts in ZF1; without the flag the receiver obeys that ZF1 option, and without one it uses `skip-identical-else-rename`. The sender side maps the policy to ZF1 (`ZMCRC` by default, `ZMCLOB`, `ZMPROT`, `ZMCHNG`). Repeats are handled idempotently: ZFILE copies the sender queued while sio was busy (for example while the Save dialog was open) are dropped instead of each getting a ZRPOS, a repeat is answered again only when it arrives well after the ZRPOS with no data yet (the answer was lost), damaged headers or stale ZDATA while a ZRPOS is out get no ZRPOS of their own, and as sender sio repeats ZFILE only while the receiver has said nothing or asked for it (ZNAK, ZRINIT). Only a ZRPOS that rewinds sent data halves the subpacket length and counts as a retransmission, and such a rewind closes the open frame with an empty ZCRCE before the fresh ZDATA header so the receiver never meets a header inside a frame; a ZRPOS at or before the start of the frame being written repeats an answer already obeyed and is left alone while that frame is open. ZABORT, ZFERR or a cancel sequence from the peer end the session (a ZABORT gets ZFIN), and after that sio writes nothing more to the link. In a batch a bad name or write failure skips that file and the rest continue; a full disk ends the session. `ZCOMMAND` is always refused. The last output line and the exit code tell the outcome apart: `result: ok` (0), `result: failed: <reason>` (1), `result: skipped` (3, the target already held the same content), `result: resumed` (4), `result: cancelled` (5), `result: refused` (6, the receiver kept its existing file without comparing, as `rz` does by default), `result: partial: <files>` (7, a batch where some files went through and others were refused or failed). A batch with any refused or failed file never ends as plain ok. A plain ZSKIP from the receiver counts as refused; only a ZSKIP after the receiver asked our ZCRC counts as identical. ZF1 is only a request: `rz` ignores it, so `sio sz --conflict` prints that the device decides with `rz -o` (overwrite) or `-n` (rename). As receiver, sio asks `ZCRC` up to 3 times, again on silence or a damaged answer, before starting from 0. As sender, a ZRPOS at the current offset while a frame is open closes it with an empty ZCRCE and sends a fresh ZDATA header; a local read error cuts that file short at the last good offset (ZCRCE, ZEOF) and the batch goes on. As receiver, ZRINIT is repeated on a 10 s timer only until the sender has spoken in that phase; after that only on request (ZRQINIT, a repeated ZEOF, a damaged header, rate limited to one per second). While a file dialog is open sio watches the link; if the device gives up the dialog is closed and nothing more is written. Ctrl+C cancels with 8 CAN plus 8 backspaces; a second Ctrl+C also stops waiting for the device's result line. Run `rz` on the device first, then `sio sz`; `sio sz` claims the port at once and waits for the device's ZRINIT. On connect, transfers first send a claim preamble to the session worker, which strips it and answers that connection with an ok or busy token. While the claim is held, writes from every other control client and shared virtual port are dropped and serial output goes to the claiming connection only; the claim ends when the transfer disconnects. A worker that does not answer the claim is an older build and must be restarted. Both commands require a running session worker. XON/XOFF must stay off.

`sio shell <session>` connects to the named session worker in the foreground when it is running. It continuously prints serial output, shows an internal `>>` input prompt, and writes submitted lines to the port. Exiting shell must leave the background session worker running. Entered lines use the same default CRLF line mode as `sio send`. Shell history is per session in `history.log`; Up/Down recall history, Left/Right move the cursor for in-line editing, gray completion suggests the first matching device candidate or the most recent matching history entry, and Right at end of line accepts the suggestion. For a top-level input prefix, every Tab must keep the local line unchanged, discard prior device candidates, and send only a bare Tab. Device output remains visible while output from the next 500 milliseconds is copied into a bounded, in-memory candidate cache; suppress local prompt redraws until that window finishes so the prompt is not inserted between device response chunks. Device candidates are one-shot for the current input line and must be discarded when another Tab starts, a suggestion is accepted, or the line is submitted or cancelled. Lines containing whitespace use history completion only. Ctrl+D and other non-editing control keys are forwarded immediately; any pending local prefix is sent before the control byte and then cleared. A standalone Escape is forwarded after a short arrow-sequence disambiguation window. Arrow keys and Backspace remain local editing keys. One Ctrl+C sends byte `0x03` to the device; a second press shortly after exits the shell. While `sio shell` runs, it scans device output for a CRC-valid hex `ZRQINIT` (the device sends) or `ZRINIT` (the device receives) header and only those; any other bytes, including a header with a bad CRC, are shown as normal output. On a header it stops showing device output and forwarding keys, claims the port, and runs the transfer: a Save As dialog prefilled with the device file name (a folder picker when the device announces several files), or a multi-select Open dialog. Dialogs use comdlg32/shell32 through `golang.org/x/sys/windows`, owned by the terminal window; the folder used last is remembered per session in `transfer_dir`. Without a desktop (SSH, other OSes) the shell asks for a path on its input line. Cancelling a dialog sends the cancel sequence. During a transfer Ctrl+C cancels the transfer instead of reaching the device or leaving the shell; further presses also skip the wait for the device result line, and none of them leave the shell. On a terminal the shell shows a two-line status panel at the bottom, redrawn in place with plain ANSI sequences (no TUI library) on a timer, not only on data, and fitted to the current width: direction arrow, file name (shortened in the middle), bar, percent and size; then rate, ETA, retransmissions (ZRPOS), CRC errors, window, CRC type and file i/N. Before data it names the step it waits on and for how long; an unknown size shows bytes and rate only. The session's log lines print above the panel. When the session ends the panel becomes the summary (outcome, files, bytes, elapsed, average rate, retransmissions, CRC errors, resume offset) and the hint `按回车返回 shell / Press Enter to return to the shell`; only Enter or Ctrl+C closes it, other keys are dropped without echo, and device output arriving meanwhile is held (bounded) and printed after the `zmodem: <outcome>` record once the panel is cleared. Without a terminal (redirected output) the shell prints plain progress lines and the record and returns at once. `sio sz` and `sio rz` keep their single-line progress. `sio shell <session> --no-auto-transfer` turns detection off, and `--raw` sessions never detect.

`sio tee <session> <file>` opens the named session port and keeps it open in the foreground. It continuously writes serial output to both the screen and the file, and also appends output to the local cache file used by `sio read <session>`.

`sio share <session> <virtual-port>...` creates the com0com virtual port pairs when `setupc.exe` is available, records the public virtual ports and private hub ports, and starts a background `sio worker` that runs the built-in Go byte bridge for that session. The bridge treats virtual COM ports as byte-stream clients. Virtual-port baud, line settings, and modem-control signals are not propagated to the physical serial port; the physical serial port is configured only by the named session state. Driver installation must remain explicit; if com0com is not installed or `setupc.exe` is not on PATH, the command should fail with an actionable error rather than silently installing a driver.

`sio tcp <session> <listen-address>` records a TCP listen address and starts a background `sio worker` that accepts TCP clients and bridges them to the named serial session.

Background workers append lifecycle and retry diagnostics to each session's `worker.log`. This file lives next to `state.json` and `cache.log` under the session directory.

`sio log <session>` prints that session's `worker.log`. Use it when diagnosing share behavior.

`sio status <session>` should expose PID liveness as `running`, `stale`, or `stopped`. `stale` means the PID is saved in state but no matching process is running; `sio stop <session>` should still clean only that session's resources without failing on the missing process.

`sio pause <session>` and `sio resume <session>` update local session state. They are preparation for burn/flash workflows and future long-running session ownership.

`sio stop <session>` stops only the named session's worker process, removes only that session's virtual port pairs, and clears live resource state. It must not stop other sessions because multiple agents may be controlling different devices.

`sio rm <session>` performs the same named-session live cleanup as `sio stop <session>`, then deletes that session directory including `state.json`, `cache.log`, `worker.log`, and extracted tools. It must not remove other sessions.

`sio clear --share` is the orphaned-virtual-port fallback: it stops every sharing session's worker, then removes all com0com virtual ports on the machine, including ones no longer tracked by any local session. Reach for it when a virtual port still exists but no session claims it, or when `stop`/`rm` reports incomplete cleanup; prefer per-session `stop`/`rm` otherwise.

## Development Rules

Write tests for behavior changes before implementation when practical. At minimum, add or update tests for command parsing, session state, serial stream behavior, and skill installation behavior.

Prefer external behavior tests under `tests/` using packages such as `cli_test`, `session_test`, and `skill_test`. Package-local tests next to implementation are acceptable for small unexported helpers that are hard to exercise without real serial hardware, such as stream copying and tee/cache file fan-out.

Default unit tests must never trigger Windows UAC prompts, elevated child processes, `setupc.exe`, `pnputil`, PnP device removal/restart, registry writes such as ComDB edits, `QueryDosDevice`-driven cleanup, or real serial-port open/delete operations. Those paths require explicit real-machine validation with the human present. Keep automated tests for these areas limited to pure parsing, state transitions, command construction, and fully stubbed OS boundaries; `go test ./...` must not spawn admin windows or mutate real devices.

Run these before claiming work is complete:

```bash
go test ./...
go build -o bin/sio.exe ./cmd/sio
go build -o bin/gs.exe ./cmd/gs
```

For CLI behavior changes, completion also requires deployment to the active
local command on `PATH`. Do not hand off after only building `bin/*.exe`; install
the binary and verify `Get-Command` plus `version` for the command the human will
run. When the compatibility alias is affected, install and verify both `sio` and
`gs`.

For local installation during development:

```powershell
$commit = git rev-parse --short HEAD
if (git status --porcelain) { $commit = "$commit-dirty" }
$builtAt = Get-Date -Format "yyyy-MM-ddTHH:mm:sszzz"
go install -ldflags "-X go-serial-cli/internal/cli.BuildVersion=dev -X go-serial-cli/internal/cli.BuildCommit=$commit -X go-serial-cli/internal/cli.BuildBuiltAt=$builtAt" ./cmd/sio
# Optional legacy alias:
go install -ldflags "-X go-serial-cli/internal/cli.BuildVersion=dev -X go-serial-cli/internal/cli.BuildCommit=$commit -X go-serial-cli/internal/cli.BuildBuiltAt=$builtAt" ./cmd/gs
```

On Windows, when `GOBIN` is unset, Go installs `sio.exe` to:

```text
%GOPATH%\bin\sio.exe
```

For the current local user, this normally resolves to:

```text
C:\Users\<you>\go\bin\sio.exe
```

Ensure `%GOPATH%\bin` is on `PATH` before expecting `sio` to be available from any terminal.

When asking a human to manually test a CLI behavior change, install the binary first. Building
`bin/sio.exe` is not enough because terminals normally run the `sio.exe` found on `PATH`.
After installing, always verify which binary will run before handing off manual testing.

After installing, verify the active binary with:

```powershell
Get-Command sio
sio version
# Optional legacy alias check:
Get-Command gs
gs version
```

For command checks on Windows:

```bash
go run ./cmd/sio version
go run ./cmd/sio help
go run ./cmd/sio ports
go run ./cmd/sio skill install --to ./.tmp-skills
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

## OSW Provider Policy

| Task type | Provider | Model | Thinking |
|---|---|---|---|
| code (implementation) | claude | claude-opus-5-5[1m] | high |
