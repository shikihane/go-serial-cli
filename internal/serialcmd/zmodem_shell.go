package serialcmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FilePicker chooses local files for a transfer the device started. The
// Windows implementation shows the common dialogs; without a desktop the
// shell asks for a path on its own input line.
type FilePicker interface {
	// OpenFiles picks one or more files to send.
	OpenFiles(dir string) ([]string, error)
	// SaveFile picks where one received file goes, starting from name.
	SaveFile(dir, name string) (string, error)
	// PickFolder picks the directory for a batch of received files.
	PickFolder(dir string) (string, error)
}

// PickerCloser is implemented by a FilePicker whose open dialog can be
// closed from outside, as if Cancel was pressed.
type PickerCloser interface {
	Close()
}

var (
	// ErrPickCancelled means the person closed the picker.
	ErrPickCancelled = errors.New("file selection cancelled")
	// ErrPickerUnavailable means no dialog can be shown here.
	ErrPickerUnavailable = errors.New("file dialog unavailable")
)

// ShellTransferOptions turn on ZMODEM auto-detection in sio shell.
type ShellTransferOptions struct {
	Picker      FilePicker
	LastDir     func() string
	SaveLastDir func(dir string)
	Conflict    ConflictPolicy
	Window      int
	// Active tells the input layer that a transfer owns the keyboard:
	// Ctrl+C then cancels the transfer instead of counting toward exit.
	Active func(active bool)
	// HandshakeWait bounds the wait for the device once the picker is
	// done. The device itself waits for the person.
	HandshakeWait time.Duration
	// Width reports the terminal width in columns; 0, or no Width, means
	// the output is no terminal. On a terminal a transfer shows a status
	// panel, and its summary stays until Enter is pressed.
	Width func() int
	// RawKeys is called with true while the panel owns the keyboard:
	// keys must then reach the shell input as typed, with no local echo
	// or line editing. It is called with false while an inline prompt
	// needs the line editor, and when the panel is gone.
	RawKeys func(raw bool)
}

const (
	shellTransferHandshakeWait = 20 * time.Second
	shellTransferCooldown      = 1500 * time.Millisecond
	shellDetectIdleFlush       = 50 * time.Millisecond
	shellReleaseWait           = time.Second
)

// zDetector finds a CRC-valid hex ZRQINIT or ZRINIT in shell output. Bytes
// that could still become such a header are held back until they resolve.
type zDetector struct {
	hold []byte
}

const zDetectLen = 4 + 14 // "**", ZDLE, 'B', 14 hex digits

// feed returns the bytes to show and, when a header was found, its type.
// Bytes after the header belong to the transfer and are dropped.
func (d *zDetector) feed(data []byte) ([]byte, byte, bool) {
	buf := append(d.hold, data...)
	d.hold = nil
	for i := 0; i < len(buf); i++ {
		if buf[i] != zPAD {
			continue
		}
		cand := buf[i:]
		// One or two ZPAD, then ZDLE 'B'.
		j := 1
		if len(cand) > 1 && cand[1] == zPAD {
			j = 2
		}
		need := j + 2 + 14
		if len(cand) < need {
			if zCouldBeHexHeader(cand, j) {
				d.hold = append([]byte(nil), cand...)
				return buf[:i], 0, false
			}
			continue
		}
		if cand[j] != zDLE || cand[j+1] != zHEX {
			continue
		}
		h, ok := decodeHexHeaderDigits(cand[j+2 : j+2+14])
		if !ok || (h.Type != zRQINIT && h.Type != zRINIT) {
			continue
		}
		show := buf[:i]
		// lrzsz and the device send "rz\r" before ZRQINIT.
		show = bytes.TrimSuffix(show, []byte("rz\r"))
		return show, h.Type, true
	}
	return buf, 0, false
}

func zCouldBeHexHeader(cand []byte, pads int) bool {
	for k := pads; k < len(cand); k++ {
		switch {
		case k == pads && cand[k] != zDLE:
			return false
		case k == pads+1 && cand[k] != zHEX:
			return false
		case k > pads+1:
			if _, ok := zHexValue(cand[k]); !ok {
				return false
			}
		}
	}
	return true
}

// flush returns held bytes once the output went idle without completing
// a header.
func (d *zDetector) flush() []byte {
	out := d.hold
	d.hold = nil
	return out
}

// shellGate routes keyboard input while a transfer runs: Ctrl+C cancels
// it, and whole lines go to an inline picker prompt if one waits.
type shellGate struct {
	mu     sync.Mutex
	active bool
	cancel chan struct{}
	once   *sync.Once
	// force closes on a second Ctrl+C: stop waiting for the device.
	force     chan struct{}
	forceOnce *sync.Once
	presses   int
	// prompt closes when the inline prompt must give up.
	prompt     chan struct{}
	promptOnce *sync.Once
	lines      chan string
	line       []byte
	// dismiss, when set, closes on Enter or Ctrl+C while the summary is
	// shown; every other key is dropped.
	dismiss     chan struct{}
	dismissOnce *sync.Once
}

func newShellGate() *shellGate {
	return &shellGate{lines: make(chan string, 4)}
}

func (g *shellGate) begin() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active = true
	g.cancel = make(chan struct{})
	g.once = &sync.Once{}
	g.force = make(chan struct{})
	g.forceOnce = &sync.Once{}
	g.presses = 0
	g.prompt = make(chan struct{})
	g.promptOnce = &sync.Once{}
	g.dismiss, g.dismissOnce = nil, nil
	g.line = g.line[:0]
	for len(g.lines) > 0 {
		<-g.lines
	}
	return g.cancel
}

func (g *shellGate) forced() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.force
}

func (g *shellGate) closePrompt() {
	g.mu.Lock()
	once, ch := g.promptOnce, g.prompt
	g.mu.Unlock()
	if once != nil {
		once.Do(func() { close(ch) })
	}
}

func (g *shellGate) promptDone() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.prompt
}

func (g *shellGate) end() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active = false
	g.dismiss, g.dismissOnce = nil, nil
}

// awaitDismiss hands the keyboard to the summary: the returned channel
// closes on Enter or Ctrl+C typed from now on.
func (g *shellGate) awaitDismiss() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dismiss, g.dismissOnce = make(chan struct{}), &sync.Once{}
	return g.dismiss
}

// feed takes input bytes. It returns false when the gate is open and the
// bytes should go to the device.
func (g *shellGate) feed(data []byte) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.active {
		return false
	}
	if g.dismiss != nil {
		for _, b := range data {
			if b == '\r' || b == '\n' || b == 0x03 {
				once, ch := g.dismissOnce, g.dismiss
				once.Do(func() { close(ch) })
			}
		}
		return true
	}
	for _, b := range data {
		switch b {
		case 0x03:
			// The first press cancels the transfer; any later one also
			// stops waiting for the device's result line. None of them
			// leave the shell.
			g.presses++
			once, ch := g.once, g.cancel
			once.Do(func() { close(ch) })
			if g.presses > 1 {
				fonce, fch := g.forceOnce, g.force
				fonce.Do(func() { close(fch) })
			}
		case '\r', '\n':
			select {
			case g.lines <- string(g.line):
			default:
			}
			g.line = g.line[:0]
		case 0x08, 0x7f:
			if len(g.line) > 0 {
				g.line = g.line[:len(g.line)-1]
			}
		default:
			if b >= 0x20 {
				g.line = append(g.line, b)
			}
		}
	}
	return true
}

// inlinePicker asks for paths on the shell input line.
type inlinePicker struct {
	out    io.Writer
	lines  <-chan string
	cancel <-chan struct{}
	closed <-chan struct{}
	// asking, when set, is called with true before the question and with
	// false once it is answered.
	asking func(bool)
}

func (p *inlinePicker) ask(question string) (string, error) {
	if p.asking != nil {
		p.asking(true)
		defer p.asking(false)
	}
	_, _ = fmt.Fprintf(p.out, "\r\nzmodem: %s\r\n", question)
	select {
	case line := <-p.lines:
		line = strings.TrimSpace(line)
		if line == "-" {
			return "", ErrPickCancelled
		}
		return strings.Trim(line, `"`), nil
	case <-p.cancel:
		return "", ErrPickCancelled
	case <-p.closed:
		_, _ = fmt.Fprint(p.out, "zmodem: the device gave up; prompt closed\r\n")
		return "", ErrPickCancelled
	}
}

func (p *inlinePicker) OpenFiles(dir string) ([]string, error) {
	line, err := p.ask("device is ready to receive; type the file path(s) to send, separated by ';' (- cancels)")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, part := range strings.Split(line, ";") {
		part = strings.Trim(strings.TrimSpace(part), `"`)
		if part == "" {
			continue
		}
		if !filepath.IsAbs(part) && dir != "" {
			if _, err := os.Stat(part); err != nil {
				part = filepath.Join(dir, part)
			}
		}
		files = append(files, part)
	}
	if len(files) == 0 {
		return nil, ErrPickCancelled
	}
	return files, nil
}

func (p *inlinePicker) SaveFile(dir, name string) (string, error) {
	def := filepath.Join(dir, name)
	line, err := p.ask(fmt.Sprintf("device is sending %s; save as? (Enter = %s, - cancels)", name, def))
	if err != nil {
		return "", err
	}
	if line == "" {
		return def, nil
	}
	if info, err := os.Stat(line); err == nil && info.IsDir() {
		return filepath.Join(line, name), nil
	}
	return line, nil
}

func (p *inlinePicker) PickFolder(dir string) (string, error) {
	line, err := p.ask(fmt.Sprintf("device is sending several files; save into which folder? (Enter = %s, - cancels)", dir))
	if err != nil {
		return "", err
	}
	if line == "" {
		return dir, nil
	}
	return line, nil
}

// shellTransfer runs the transfers a shell detects.
type shellTransfer struct {
	opts    ShellTransferOptions
	address string
	out     io.Writer
	gate    *shellGate
	run     func(ZModemOptions) error

	mu      sync.Mutex
	running bool
	ended   time.Time
	// panel is the status panel of the latest transfer on a terminal.
	panel *shellPanel
	// holding: its summary waits for Enter. released: the worker has
	// ended the claim. handed: the keyboard went back to the shell.
	holding  bool
	released bool
	handed   bool
}

// muted reports a running transfer, or one that ended moments ago and
// may still have TX records on their way to the cache.
func (t *shellTransfer) muted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running || time.Since(t.ended) < shellTransferCooldown
}

func (t *shellTransfer) setRunning(running bool) {
	t.mu.Lock()
	t.running = running
	if !running {
		t.ended = time.Now()
	}
	t.mu.Unlock()
}

// gatedReader holds keyboard input back from the device during a transfer.
type gatedReader struct {
	r    io.Reader
	gate *shellGate
}

func (g *gatedReader) Read(b []byte) (int, error) {
	for {
		n, err := g.r.Read(b)
		if n > 0 && g.gate.feed(b[:n]) {
			if err != nil {
				return 0, err
			}
			continue
		}
		return n, err
	}
}

func (t *shellTransfer) lastDir() string {
	dir := ""
	if t.opts.LastDir != nil {
		dir = t.opts.LastDir()
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	return dir
}

func (t *shellTransfer) remember(dir string) {
	if t.opts.SaveLastDir != nil && dir != "" {
		t.opts.SaveLastDir(dir)
	}
}

// pickerFor returns the dialog picker, or the inline one when no dialog
// can be shown.
func (t *shellTransfer) withPicker(cancel <-chan struct{}, use func(FilePicker) error) error {
	inline := &inlinePicker{out: t.out, lines: t.gate.lines, cancel: cancel, closed: t.gate.promptDone()}
	if panel := t.currentPanel(); panel != nil {
		// The question and the typed answer use the input line.
		inline.asking = func(asking bool) {
			if asking {
				panel.pause()
				t.rawKeys(false)
				return
			}
			t.rawKeys(true)
			panel.resume()
		}
	}
	if t.opts.Picker == nil {
		return use(inline)
	}
	err := use(t.opts.Picker)
	if errors.Is(err, ErrPickerUnavailable) {
		return use(inline)
	}
	return err
}

func (t *shellTransfer) currentPanel() *shellPanel {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.panel
}

func (t *shellTransfer) rawKeys(raw bool) {
	if t.opts.RawKeys != nil {
		t.opts.RawKeys(raw)
	}
}

func (t *shellTransfer) terminalWidth() int {
	if t.opts.Width == nil {
		return 0
	}
	return t.opts.Width()
}

// isHolding reports a summary that waits for Enter.
func (t *shellTransfer) isHolding() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.holding
}

// write shows device output, through the panel while it is up.
func (t *shellTransfer) write(out io.Writer, data []byte) error {
	if panel := t.currentPanel(); panel != nil {
		return panel.deviceOutput(data)
	}
	_, err := out.Write(data)
	return err
}

// start runs one transfer for a detected header. Device output and keys
// are held for its duration and until release is called: keys typed
// before the worker has dropped the claim would be discarded by it. On a
// terminal the status panel shows the transfer, and its summary holds
// output and keys until Enter.
func (t *shellTransfer) start(frameType byte) {
	cancel := t.gate.begin()
	t.mu.Lock()
	t.panel, t.holding, t.released, t.handed = nil, false, false, false
	t.mu.Unlock()
	t.setRunning(true)
	if t.opts.Active != nil {
		t.opts.Active(true)
	}
	wait := t.opts.HandshakeWait
	if wait <= 0 {
		wait = shellTransferHandshakeWait
	}
	var result ZModemResult
	opts := ZModemOptions{
		Address:       t.address,
		Conflict:      t.opts.Conflict,
		Window:        t.opts.Window,
		HandshakeWait: wait,
		Output:        t.out,
		Cancel:        cancel,
		Force:         t.gate.forced(),
		Result:        &result,
		ClosePicker:   t.closePicker,
	}
	if frameType == zRQINIT {
		_, _ = fmt.Fprint(t.out, "\r\nzmodem: device is sending; Ctrl+C cancels\r\n")
	} else {
		_, _ = fmt.Fprint(t.out, "\r\nzmodem: device is ready to receive; Ctrl+C cancels\r\n")
	}
	var panel *shellPanel
	if t.terminalWidth() > 0 {
		panel = newShellPanel(t.out, t.opts.Width)
		t.mu.Lock()
		t.panel = panel
		t.mu.Unlock()
		t.rawKeys(true)
		opts.Output = panel
		opts.DeviceText = panelText{panel}
		opts.Progress = panel.update
	}
	if frameType == zRQINIT {
		opts.Receive = true
		opts.Destination = t.destination(cancel)
	} else {
		opts.PickFiles = func() ([]ZSendFile, error) {
			var picked []string
			err := t.withPicker(cancel, func(p FilePicker) error {
				var err error
				picked, err = p.OpenFiles(t.lastDir())
				return err
			})
			if errors.Is(err, ErrPickCancelled) {
				return nil, ErrTransferCancelled
			}
			if err != nil {
				return nil, err
			}
			files := make([]ZSendFile, 0, len(picked))
			for _, path := range picked {
				files = append(files, ZSendFile{Path: path})
			}
			if len(picked) > 0 {
				t.remember(filepath.Dir(picked[0]))
			}
			return files, nil
		}
	}
	err := t.run(opts)
	line := "zmodem: " + result.Outcome
	if result.Outcome == "" {
		line = "zmodem: " + OutcomeFailed
	}
	if err != nil && !errors.Is(err, ErrTransferCancelled) {
		line += ": " + err.Error()
	}
	if panel == nil {
		_, _ = fmt.Fprint(t.out, line+"\r\n")
		return
	}
	// The summary stays until Enter (or Ctrl+C); output that arrives
	// meanwhile is held by the panel and shown after it.
	t.mu.Lock()
	t.holding = true
	t.mu.Unlock()
	dismiss := t.gate.awaitDismiss()
	panel.finish(panelSummary(!opts.Receive, result, err))
	go func() {
		<-dismiss
		panel.close(line + "\r\n")
		t.mu.Lock()
		t.holding = false
		t.mu.Unlock()
		t.handBack()
	}()
}

// closePicker ends a dialog or inline prompt that is still waiting for
// the person after the device gave up.
func (t *shellTransfer) closePicker() {
	t.gate.closePrompt()
	if c, ok := t.opts.Picker.(PickerCloser); ok {
		c.Close()
	}
}

// release notes that the worker has ended the claim.
func (t *shellTransfer) release() {
	t.mu.Lock()
	t.released = true
	t.mu.Unlock()
	t.handBack()
}

// handBack returns the keyboard to the shell once the worker has ended
// the claim and no summary waits for Enter, exactly once per transfer.
func (t *shellTransfer) handBack() {
	t.mu.Lock()
	ready := t.released && !t.holding && !t.handed
	if ready {
		t.handed = true
	}
	panel := t.panel
	t.mu.Unlock()
	if !ready {
		return
	}
	t.gate.end()
	t.setRunning(false)
	if panel != nil {
		t.rawKeys(false)
	}
	if t.opts.Active != nil {
		t.opts.Active(false)
	}
}

// destination asks once per session: a save dialog for one file, a folder
// for a batch. Later files go to the same folder.
func (t *shellTransfer) destination(cancel <-chan struct{}) func(ZFileInfo) (ZDestination, error) {
	dir := ""
	return func(info ZFileInfo) (ZDestination, error) {
		if dir != "" {
			return ZDestination{Path: filepath.Join(dir, info.Name)}, nil
		}
		var dest ZDestination
		err := t.withPicker(cancel, func(p FilePicker) error {
			start := t.lastDir()
			if info.FilesLeft > 1 {
				folder, err := p.PickFolder(start)
				if err != nil {
					return err
				}
				dir = folder
				dest = ZDestination{Path: filepath.Join(folder, info.Name)}
				return nil
			}
			path, err := p.SaveFile(start, info.Name)
			if err != nil {
				return err
			}
			dir = filepath.Dir(path)
			dest = ZDestination{Path: path}
			if _, inline := p.(*inlinePicker); !inline {
				// The save dialog already asked before replacing a file.
				dest.Policy = ConflictOverwrite
			}
			return nil
		})
		if errors.Is(err, ErrPickCancelled) {
			return ZDestination{}, ErrTransferCancelled
		}
		if err != nil {
			return ZDestination{}, err
		}
		t.remember(dir)
		return dest, nil
	}
}

// copyShellOutput shows device output and starts a transfer when a
// ZMODEM header appears. Bytes that arrive while a transfer runs, and the
// claim notice that ends it, are not shown.
func copyShellOutput(conn net.Conn, output io.Writer, transfer *shellTransfer) error {
	if transfer == nil {
		_, err := io.Copy(output, conn)
		return err
	}
	var det zDetector
	var cooldown time.Time
	// After a transfer, bytes up to the worker's release notice were
	// received before the claim took hold: protocol bytes, not output.
	var skipUntil time.Time
	var skipped []byte
	notice := []byte(transferReleaseNotice)
	buf := make([]byte, 4096)
	for {
		switch {
		case !skipUntil.IsZero():
			_ = conn.SetReadDeadline(skipUntil)
		case len(det.hold) > 0:
			_ = conn.SetReadDeadline(time.Now().Add(shellDetectIdleFlush))
		default:
			_ = conn.SetReadDeadline(time.Time{})
		}
		n, err := conn.Read(buf)
		data := buf[:n]
		if !skipUntil.IsZero() && n > 0 {
			skipped = append(skipped, data...)
			idx := bytes.Index(skipped, notice)
			if idx < 0 {
				// Keep enough to find a notice split across reads.
				if len(skipped) > len(notice) {
					skipped = skipped[len(skipped)-len(notice):]
				}
				data = nil
			} else {
				data = append([]byte(nil), skipped[idx+len(notice):]...)
				skipped, skipUntil = nil, time.Time{}
				transfer.release()
			}
		}
		if len(data) > 0 {
			data = bytes.ReplaceAll(data, notice, nil)
			show, frameType, found := det.feed(data)
			if len(show) > 0 {
				if werr := transfer.write(output, show); werr != nil {
					return werr
				}
			}
			if found && (time.Now().Before(cooldown) || transfer.isHolding()) {
				// A header repeated by the device after a transfer ended,
				// or one sent while the last summary is still shown.
				found = false
			}
			if found {
				transfer.start(frameType)
				det = zDetector{}
				cooldown = time.Now().Add(shellTransferCooldown)
				skipUntil = time.Now().Add(shellReleaseWait)
				skipped = nil
			}
		}
		if err != nil {
			if isTimeout(err) {
				if !skipUntil.IsZero() && !time.Now().Before(skipUntil) {
					// No notice from an older worker: release anyway.
					skipUntil, skipped = time.Time{}, nil
					transfer.release()
				}
				if held := det.flush(); len(held) > 0 {
					if werr := transfer.write(output, held); werr != nil {
						return werr
					}
				}
				continue
			}
			return err
		}
	}
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
