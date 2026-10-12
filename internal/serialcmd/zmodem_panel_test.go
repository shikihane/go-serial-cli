package serialcmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// panelRig is a shell rig whose output is a terminal of a given width.
type panelRig struct {
	*shellRig
	width atomic.Int32
	rawMu sync.Mutex
	raw   []bool
}

func startPanelRig(t *testing.T, picker FilePicker, width int, wire ...zWireConfig) *panelRig {
	t.Helper()
	pr := &panelRig{}
	pr.width.Store(int32(width))
	pr.shellRig = startShellRig(t, picker, func(o *ShellTransferOptions) {
		o.Width = func() int { return int(pr.width.Load()) }
		o.RawKeys = func(raw bool) {
			pr.rawMu.Lock()
			pr.raw = append(pr.raw, raw)
			pr.rawMu.Unlock()
		}
	}, wire...)
	return pr
}

func (r *panelRig) raws() []bool {
	r.rawMu.Lock()
	defer r.rawMu.Unlock()
	return append([]bool(nil), r.raw...)
}

// drainDevice collects what the shell sends to the device from now on.
func (r *shellRig) drainDevice() *syncBuffer {
	got := &syncBuffer{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.dev.Read(buf)
			if n > 0 {
				_, _ = got.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return got
}

// panelRows returns the rows of the last panel drawn in out: the text
// after the last clear-to-end-of-screen.
func panelRows(out string) []string {
	idx := strings.LastIndex(out, "\x1b[J")
	if idx < 0 {
		return nil
	}
	return strings.Split(out[idx+len("\x1b[J"):], "\r\n")
}

func TestShellPanelShowsProgressThenHoldsSummaryUntilEnter(t *testing.T) {
	dir := t.TempDir()
	save := filepath.Join(dir, "clip.wav")
	// Two files: the shell asks for a folder.
	rig := startPanelRig(t, &fakePicker{folder: dir}, 80, zWireConfig{rate: 200 << 10})
	data := zPattern(500_000, 41)
	devErr := make(chan error, 1)
	go func() {
		_, err := rig.deviceTransfer(ZModemOptions{Files: []ZSendFile{{Name: "clip.wav", Data: data}, {Name: "b.txt", Data: []byte("tail")}}})
		devErr <- err
	}()

	// While data flows: both panel rows, redrawn in place.
	var during []string
	waitFor(t, "panel progress rows", 10*time.Second, func() bool {
		during = panelRows(rig.out.String())
		return len(during) == 2 && strings.Contains(during[0], "[#") && strings.Contains(during[1], "ETA")
	})
	if !strings.HasPrefix(during[0], panelArrowRecv+" clip.wav  [#") || !strings.Contains(during[0], "KB/") {
		t.Fatalf("progress row 1 = %q", during[0])
	}
	for _, want := range []string{"KB/s", "retrans 0", "CRC err 0", "W -", "CRC32", "file 1/2"} {
		if !strings.Contains(during[1], want) {
			t.Fatalf("progress row 2 = %q, want %q", during[1], want)
		}
	}
	for _, row := range during {
		if w := displayWidth(row); w > 79 {
			t.Fatalf("row of %d cells on an 80 column terminal: %q", w, row)
		}
	}
	if strings.Contains(rig.out.String(), "zmodem: recv ") {
		t.Fatalf("plain progress lines printed under the panel:\n%q", rig.out.String())
	}
	if err := <-devErr; err != nil {
		t.Fatalf("device send: %v\n%q", err, rig.out.String())
	}
	mustEqualFile(t, save, data)

	devGot := rig.drainDevice()
	rig.deviceWrite("done ok=2\r\ndev$ ")
	waitFor(t, "summary with hint", 5*time.Second, func() bool { return strings.Contains(rig.out.String(), panelHint) })
	time.Sleep(300 * time.Millisecond)
	out := rig.out.String()
	rows := panelRows(out)
	if len(rows) != 3 {
		t.Fatalf("summary rows = %q", rows)
	}
	if !strings.HasPrefix(rows[0], panelArrowRecv+" ok  clip.wav -> ") || !strings.Contains(rows[0], "b.txt") {
		t.Fatalf("summary row 1 = %q", rows[0])
	}
	if !strings.Contains(rows[1], "500004 bytes") || !strings.Contains(rows[1], "avg ") || !strings.Contains(rows[1], "retrans 0") {
		t.Fatalf("summary row 2 = %q", rows[1])
	}
	if rows[2] != "\x1b[7m"+panelHint+"\x1b[0m" {
		t.Fatalf("hint row = %q", rows[2])
	}
	if strings.Contains(out, "done ok=2") || strings.Contains(out, "zmodem: ok") {
		t.Fatalf("device output or record shown over the summary:\n%q", out)
	}

	// Keys other than Enter go nowhere, and the shell keeps the keyboard.
	_, _ = rig.input.Write([]byte("abc\x1b[A"))
	time.Sleep(300 * time.Millisecond)
	if bytes.Contains([]byte(devGot.String()), []byte("abc")) {
		t.Fatalf("keys reached the device before Enter: %q", devGot.String())
	}
	if a := rig.actives(); len(a) != 1 || !a[0] {
		t.Fatalf("active calls before Enter = %v", a)
	}
	rig.deviceWrite("late line\r\n")
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(rig.out.String(), "late line") {
		t.Fatal("device output drawn while the summary is shown")
	}

	_, _ = rig.input.Write([]byte("\r"))
	waitFor(t, "held output after Enter", 3*time.Second, func() bool { return strings.Contains(rig.out.String(), "late line") })
	waitFor(t, "keyboard back", 3*time.Second, func() bool { a := rig.actives(); return len(a) == 2 && !a[1] })
	out = rig.out.String()
	after := out[strings.LastIndex(out, panelHint):]
	iClear := strings.Index(after, "\r\x1b[2A\x1b[J")
	iRecord := strings.Index(after, "zmodem: ok\r\n")
	iDone := strings.Index(after, "done ok=2")
	iLate := strings.Index(after, "late line")
	if iClear < 0 || iRecord < iClear || iDone < iRecord || iLate < iDone {
		t.Fatalf("after Enter want clear, record, held output in order:\n%q", after)
	}
	if r := rig.raws(); len(r) != 2 || !r[0] || r[1] {
		t.Fatalf("raw key calls = %v", r)
	}

	_, _ = rig.input.Write([]byte("help\n"))
	waitFor(t, "typed line at the device", 3*time.Second, func() bool { return strings.Contains(devGot.String(), "help\r\n") })
	if strings.Contains(devGot.String(), "abc") {
		t.Fatalf("keys typed under the summary reached the device: %q", devGot.String())
	}
}

func TestShellPanelCtrlCClosesCancelledSummary(t *testing.T) {
	rig := startPanelRig(t, &fakePicker{err: ErrPickCancelled}, 80)
	res, err := rig.deviceTransfer(ZModemOptions{Files: []ZSendFile{{Name: "x.bin", Data: []byte("x")}}})
	if !errors.Is(err, ErrTransferCancelled) || res.Outcome != OutcomeCancelled {
		t.Fatalf("device err=%v outcome=%s", err, res.Outcome)
	}
	waitFor(t, "summary with hint", 5*time.Second, func() bool { return strings.Contains(rig.out.String(), panelHint) })
	rows := panelRows(rig.out.String())
	if len(rows) != 3 || !strings.HasPrefix(rows[0], panelArrowRecv+" cancelled") {
		t.Fatalf("summary rows = %q", rows)
	}
	devGot := rig.drainDevice()
	_, _ = rig.input.Write([]byte{0x03})
	waitFor(t, "keyboard back", 5*time.Second, func() bool { a := rig.actives(); return len(a) == 2 && !a[1] })
	if !strings.Contains(rig.out.String(), "zmodem: cancelled\r\n") {
		t.Fatalf("no record after Ctrl+C:\n%q", rig.out.String())
	}
	if strings.Contains(devGot.String(), "\x03") {
		t.Fatalf("the Ctrl+C that closed the summary reached the device: %q", devGot.String())
	}
	_, _ = rig.input.Write([]byte("ls\n"))
	waitFor(t, "typed line at the device", 3*time.Second, func() bool { return strings.Contains(devGot.String(), "ls\r\n") })
}

// Without a terminal the shell keeps plain lines: no ANSI, no panel, and
// no Enter needed to get the keyboard back.
func TestShellPanelPlainFallbackWithoutTerminal(t *testing.T) {
	dir := t.TempDir()
	rig := startPanelRig(t, &fakePicker{save: filepath.Join(dir, "a.bin")}, 0, zWireConfig{rate: 200 << 10})
	if _, err := rig.deviceTransfer(ZModemOptions{Files: []ZSendFile{{Name: "a.bin", Data: zPattern(400_000, 42)}}}); err != nil {
		t.Fatalf("device send: %v", err)
	}
	waitFor(t, "keyboard back without Enter", 5*time.Second, func() bool { a := rig.actives(); return len(a) == 2 && !a[1] })
	out := rig.out.String()
	if strings.Contains(out, "\x1b") || strings.Contains(out, panelHint) {
		t.Fatalf("terminal sequences or panel without a terminal:\n%q", out)
	}
	if !strings.Contains(out, "zmodem: recv a.bin ") || !strings.Contains(out, "zmodem: ok\r\n") {
		t.Fatalf("plain progress or record missing:\n%q", out)
	}
	if r := rig.raws(); len(r) != 0 {
		t.Fatalf("raw keys without a panel: %v", r)
	}
}

func TestShellPanelUnknownSizeAndNarrowTerminal(t *testing.T) {
	dir := t.TempDir()
	name := "a-rather-long-recording-name-from-the-device-2026-10-12.wav"
	rig := startPanelRig(t, &fakePicker{save: filepath.Join(dir, "rec.wav")}, 40, zWireConfig{rate: 200 << 10})
	devErr := make(chan error, 1)
	go func() {
		_, err := rig.deviceTransfer(ZModemOptions{Files: []ZSendFile{{Name: name, Data: zPattern(500_000, 43)}}, noSize: true})
		devErr <- err
	}()
	var rows []string
	waitFor(t, "data rows", 10*time.Second, func() bool {
		rows = panelRows(rig.out.String())
		return len(rows) == 2 && strings.Contains(rows[0], " KB") && strings.Contains(rows[1], "KB/s")
	})
	if strings.Contains(rows[0], "[") || strings.Contains(rows[0], "%") || strings.Contains(rows[1], "ETA") {
		t.Fatalf("unknown size shows bar, percent or ETA: %q", rows)
	}
	if !strings.Contains(rows[0], "...") || !strings.Contains(rows[0], ".wav") {
		t.Fatalf("long name not shortened in the middle: %q", rows[0])
	}
	if err := <-devErr; err != nil {
		t.Fatalf("device send: %v", err)
	}
	waitFor(t, "summary", 5*time.Second, func() bool { return strings.Contains(rig.out.String(), "\x1b[7m") })
	out := rig.out.String()
	for _, chunk := range strings.Split(out, "\x1b[J")[1:] {
		for _, row := range strings.FieldsFunc(chunk, func(r rune) bool { return r == '\n' }) {
			if strings.HasPrefix(row, "zmodem: ") {
				// A log line printed above the panel, not a panel row.
				continue
			}
			row = strings.TrimSuffix(row, "\r")
			row = strings.TrimSuffix(strings.TrimPrefix(row, "\x1b[7m"), "\x1b[0m")
			if idx := strings.Index(row, "\r"); idx >= 0 {
				row = row[:idx]
			}
			if w := displayWidth(row); w > 39 {
				t.Fatalf("row of %d cells on a 40 column terminal: %q", w, row)
			}
		}
	}
	_, _ = rig.input.Write([]byte("\r"))
	waitFor(t, "keyboard back", 3*time.Second, func() bool { a := rig.actives(); return len(a) == 2 && !a[1] })
}

func TestPanelProgressLinesLayout(t *testing.T) {
	z := ZProgress{Send: true, Phase: ZPhaseData, Name: "song.wav", Done: 3 << 20, Total: 12 << 20,
		File: 2, Files: 3, Retransmits: 4, CRCErrors: 1, Window: 16384, CRC32: true}
	l1, l2 := panelProgressLines(z, 300*1024, 0, 100)
	if !strings.HasPrefix(l1, panelArrowSend+" song.wav  [#######") || !strings.HasSuffix(l1, " 25%  3.0 MB/12.0 MB") {
		t.Fatalf("line 1 = %q", l1)
	}
	if l2 != "300.0 KB/s  ETA 0:31  retrans 4  CRC err 1  W 16384  CRC32  file 2/3" {
		t.Fatalf("line 2 = %q", l2)
	}
	z.Total, z.Files, z.CRC32 = -1, 0, false
	l1, l2 = panelProgressLines(z, 300*1024, 0, 100)
	if l1 != panelArrowSend+" song.wav  3.0 MB" || l2 != "300.0 KB/s  retrans 4  CRC err 1  W 16384  CRC16  file 2" {
		t.Fatalf("unknown size = %q / %q", l1, l2)
	}
	// A stall shows no rate and no ETA rather than a stale one.
	z.Total = 12 << 20
	if _, l2 = panelProgressLines(z, 0, 0, 100); !strings.HasPrefix(l2, "-- KB/s  ETA --:--") {
		t.Fatalf("stalled line 2 = %q", l2)
	}
	// Before data the phase is spelled out with the time waited.
	z = ZProgress{Send: true, Phase: ZPhaseCheck, Name: "song.wav", File: 1, Files: 1, Window: 16384, CRC32: true}
	l1, l2 = panelProgressLines(z, 0, 75*time.Second, 100)
	if l1 != panelArrowSend+" song.wav  device is comparing its copy (CRC)" || !strings.HasPrefix(l2, "waited 1:15  retrans 0") {
		t.Fatalf("check phase = %q / %q", l1, l2)
	}
	z = ZProgress{Phase: ZPhaseHandshake}
	if l1, l2 = panelProgressLines(z, 0, 3*time.Second, 100); l1 != panelArrowRecv+" waiting for the device to send" || l2 != "waited 0:03" {
		t.Fatalf("handshake = %q / %q", l1, l2)
	}
}

func TestPanelProgressLinesFitAnyWidth(t *testing.T) {
	names := []string{"a.wav", "a-very-long-file-name-recorded-on-the-device-001.wav", "\u5f55\u97f3\u6587\u4ef6\u5f55\u97f3\u6587\u4ef6\u5f55\u97f3.wav"}
	for _, name := range names {
		for _, total := range []int64{-1, 1 << 30} {
			for width := 8; width <= 120; width++ {
				z := ZProgress{Phase: ZPhaseData, Name: name, Done: 1 << 29, Total: total, File: 10, Files: 12, Window: 32768, CRC32: true}
				l1, l2 := panelProgressLines(z, 1<<20, 0, width)
				for _, l := range []string{l1, l2} {
					if displayWidth(l) > width-1 {
						t.Fatalf("width %d name %q: %d cells: %q", width, name, displayWidth(l), l)
					}
				}
				if width >= 40 && !strings.HasSuffix(strings.Fields(l1)[1], ".wav") {
					t.Fatalf("width %d: name lost its extension: %q", width, l1)
				}
			}
		}
	}
}

func TestPanelSummaryLines(t *testing.T) {
	res := ZModemResult{Outcome: OutcomePartial, Files: []ZFileResult{
		{Name: "a.wav", Path: "C:/dl/a.wav", Bytes: 1000, Elapsed: time.Second, Status: OutcomeOK, Retransmits: 2},
		{Name: "b.wav", Path: "C:/dl/b.wav", Status: OutcomeRefused, Reason: "exists"},
		{Name: "c.wav", Path: "C:/dl/c.wav", Bytes: 24, Elapsed: time.Second, Status: OutcomeResumed, ResumeFrom: 500, CRCErrors: 1},
	}}
	lines := panelSummary(false, res, errors.New("1 of 3 file(s) not transferred"))
	if len(lines) != 3 || !lines[2].hint || lines[2].text != panelHint {
		t.Fatalf("summary = %+v", lines)
	}
	want := panelArrowRecv + " partial  a.wav -> C:/dl/a.wav ok, b.wav -> C:/dl/b.wav refused, c.wav -> C:/dl/c.wav resumed"
	if lines[0].text != want {
		t.Fatalf("line 1 = %q", lines[0].text)
	}
	if got := strings.Join(lines[1].parts, "  "); got != "1024 bytes (1.0 KB) in 2s  avg 512 B/s  retrans 2  CRC err 1  resumed from 500" {
		t.Fatalf("line 2 = %q", got)
	}
	lines = panelSummary(true, ZModemResult{Outcome: OutcomeFailed, Files: []ZFileResult{{Name: "m.mp4", Status: OutcomeFailed, Bytes: 15872}}},
		errors.New("receiver stopped acknowledging data"))
	if lines[0].text != panelArrowSend+" failed  m.mp4  (receiver stopped acknowledging data)" {
		t.Fatalf("failed line 1 = %q", lines[0].text)
	}
	lines = panelSummary(true, ZModemResult{Outcome: OutcomeCancelled}, ErrTransferCancelled)
	if lines[0].text != panelArrowSend+" cancelled" {
		t.Fatalf("cancelled line 1 = %q", lines[0].text)
	}
	lines = panelSummary(true, ZModemResult{Outcome: OutcomeCancelled}, fmt.Errorf("%w by the peer", ErrTransferCancelled))
	if lines[0].text != panelArrowSend+" cancelled  (transfer cancelled by the peer)" {
		t.Fatalf("peer cancelled line 1 = %q", lines[0].text)
	}
}

// When the terminal gets narrower the rows drawn before may have been
// reflowed; the redraw moves up over all of them.
func TestShellPanelRedrawAfterWidthChange(t *testing.T) {
	out := &syncBuffer{}
	var width atomic.Int32
	width.Store(100)
	p := newShellPanel(out, func() int { return int(width.Load()) })
	defer p.stopTicker()
	p.update(ZProgress{Phase: ZPhaseData, Name: "a.wav", Done: 10, Total: 100, File: 1, Files: 1, Window: 16384, CRC32: true})
	waitFor(t, "first draw", 2*time.Second, func() bool { return strings.Contains(out.String(), "\x1b[J") })
	p.mu.Lock()
	rows := append([]int(nil), p.rows...)
	p.mu.Unlock()
	if len(rows) != 2 || rows[0] <= 40 {
		t.Fatalf("rows at 100 columns = %v", rows)
	}
	up := 0
	for _, w := range rows {
		up += (w + 39) / 40
	}
	up--
	mark := len(out.String())
	width.Store(40)
	want := fmt.Sprintf("\r\x1b[%dA\x1b[J", up)
	waitFor(t, "redraw at 40 columns", 2*time.Second, func() bool { return strings.Contains(out.String()[mark:], want) })
	for _, row := range panelRows(out.String()) {
		if displayWidth(row) > 39 {
			t.Fatalf("row too wide after the resize: %q", row)
		}
	}
}

func TestFitMiddleKeepsEnds(t *testing.T) {
	if got := fitMiddle("abcdefghijklmnop.wav", 12); got != "abcd...p.wav" {
		t.Fatalf("fitMiddle = %q", got)
	}
	cjk := "\u5f55\u97f3\u6587\u4ef6.wav"
	if got := fitMiddle(cjk, 9); displayWidth(got) > 9 || !strings.HasSuffix(got, ".wav") {
		t.Fatalf("fitMiddle cjk = %q (%d cells)", got, displayWidth(got))
	}
	// Five wide characters, then " shell / Press Enter to return to the shell".
	if displayWidth(panelHint) != 53 {
		t.Fatalf("hint width = %d", displayWidth(panelHint))
	}
}

// The board case: the device stops acknowledging inside the first window.
// The old progress line only printed on data, so nothing showed for the
// whole stall; the panel keeps counting.
func TestShellPanelKeepsUpdatingWhileTheDeviceStalls(t *testing.T) {
	src := writeTemp(t, t.TempDir(), "clip.mp4", zPattern(2_000_000, 44))
	// Device to host: the ZRINIT and first ZRPOS pass, then nothing.
	toHost := zWireConfig{fault: func(off int64, chunk []byte) []byte {
		if off > 400 {
			return nil
		}
		return chunk
	}}
	portEnd, dev := newZWire(zWireConfig{rate: 300_000}, toHost)
	address := freeTCPAddress(t)
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunSessionServer(SessionServerOptions{ControlAddress: address, Port: "COM9", Baud: 3000000, Stop: stop,
			OpenPort: func(string, int) (SerialPort, error) { return portEnd, nil }})
	}()
	t.Cleanup(func() { close(stop); <-errCh })
	waitForTCPServer(t, address)
	out := &syncBuffer{}
	inR, inW := io.Pipe()
	t.Cleanup(func() { _ = inW.Close() })
	go func() {
		_ = StreamShell(ShellStreamOptions{Address: address, Input: inR, Output: out, Transfer: &ShellTransferOptions{
			Picker: &fakePicker{open: []string{src}}, HandshakeWait: 5 * time.Second, Width: func() int { return 100 }}})
	}()
	time.Sleep(100 * time.Millisecond)
	d := newRigDev(dev)
	view, detach := d.view()
	defer detach()
	go func() {
		_ = TransferZModem(ZModemOptions{Link: view, Receive: true, DestDir: t.TempDir(), RecvBuffer: 16384,
			DataWait: 30 * time.Second, HandshakeWait: 10 * time.Second, noDrain: true})
	}()
	waitFor(t, "data rows", 5*time.Second, func() bool {
		rows := panelRows(out.String())
		return len(rows) == 2 && strings.Contains(rows[0], "[")
	})
	// No data moves now, yet the panel is redrawn and the rate drops.
	waitFor(t, "stalled rate", 6*time.Second, func() bool {
		rows := panelRows(out.String())
		return len(rows) == 2 && strings.HasPrefix(rows[1], "-- KB/s  ETA --:--")
	})
	draws := strings.Count(out.String(), "\x1b[J")
	time.Sleep(time.Second)
	if n := strings.Count(out.String(), "\x1b[J"); n < draws+2 {
		t.Fatalf("panel redrawn %d times in a second of stall", n-draws)
	}
	if strings.Contains(out.String(), "zmodem: failed") {
		t.Fatal("the sender gave up before the stall was shown")
	}
	_, _ = inW.Write([]byte{0x03})
	waitFor(t, "cancelled summary", 10*time.Second, func() bool { return strings.Contains(out.String(), panelHint) })
}
