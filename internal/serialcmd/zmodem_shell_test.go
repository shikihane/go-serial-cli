package serialcmd

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestZDetectorFindsOnlyValidHexHeaders(t *testing.T) {
	zrqinit := string(encodeHexHeader(zPosHeader(zRQINIT, 0)))
	zrinit := string(encodeHexHeader(zFlagsHeader(zRINIT, zCanFDX|zCanOVIO|zCanFC32, 0, 0, 0)))
	badCRC := strings.Replace(zrinit, zrinit[len(zrinit)-7:len(zrinit)-3], "0000", 1)
	zfin := string(encodeHexHeader(zPosHeader(zFIN, 0)))
	cases := []struct {
		name  string
		input string
		want  byte
		found bool
		show  string
	}{
		{"zrqinit-after-rz", "dev$ sz a.wav\r\nrz\r" + zrqinit, zRQINIT, true, "dev$ sz a.wav\r\n"},
		{"zrinit", "ready\r\n" + zrinit + "more", zRINIT, true, "ready\r\n"},
		{"bad-crc", "x" + badCRC + "y", 0, false, "x" + badCRC + "y"},
		{"other-frame-type", zfin, 0, false, zfin},
		{"text-prefix", "**\x18B is not a header\r\n", 0, false, "**\x18B is not a header\r\n"},
		{"ansi", "\x1b[1;32mdev$ \x1b[m**** stars ****\r\n", 0, false, "\x1b[1;32mdev$ \x1b[m**** stars ****\r\n"},
		{"hex-like-text", "**\x18B00000000000001\r\n", 0, false, "**\x18B00000000000001\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var d zDetector
			show, typ, found := d.feed([]byte(tc.input))
			show = append(show, d.flush()...)
			if found != tc.found || typ != tc.want {
				t.Fatalf("found=%v type=%d, want %v %d", found, typ, tc.found, tc.want)
			}
			if string(show) != tc.show {
				t.Fatalf("show = %q, want %q", show, tc.show)
			}
		})
	}
}

func TestZDetectorHeaderSplitAcrossReads(t *testing.T) {
	stream := []byte("log line\r\n" + string(encodeHexHeader(zPosHeader(zRQINIT, 0))))
	for split := 1; split < len(stream); split++ {
		var d zDetector
		show1, typ, found1 := d.feed(stream[:split])
		var show2 []byte
		found2 := false
		if !found1 {
			// After a header the shell stops feeding: the rest is protocol.
			show2, typ, found2 = d.feed(stream[split:])
		}
		if found1 && split < len("log line\r\n")+zDetectLen {
			t.Fatalf("split %d: found before the header was complete", split)
		}
		if !(found1 || found2) || (found2 && typ != zRQINIT) {
			t.Fatalf("split %d: header not found", split)
		}
		if got := string(show1) + string(show2); got != "log line\r\n" {
			t.Fatalf("split %d: show = %q", split, got)
		}
	}
}

func TestZDetectorIgnoresRandomBinary(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	data := make([]byte, 4<<20)
	_, _ = r.Read(data)
	// Seed many header-like prefixes into the noise.
	for i := 0; i+18 < len(data); i += 997 {
		copy(data[i:], "**\x18B")
	}
	var d zDetector
	var shown int
	for off := 0; off < len(data); off += 4096 {
		show, _, found := d.feed(data[off:min(off+4096, len(data))])
		if found {
			t.Fatalf("random data triggered a transfer near %d", off)
		}
		shown += len(show)
	}
	shown += len(d.flush())
	if shown != len(data) {
		t.Fatalf("shown %d of %d bytes", shown, len(data))
	}
}

// fakePicker records calls and answers from fields.
type fakePicker struct {
	mu       sync.Mutex
	calls    []string
	open     []string
	save     string
	folder   string
	err      error
	lastDirs []string
}

func (p *fakePicker) note(call, dir string) {
	p.mu.Lock()
	p.calls = append(p.calls, call)
	p.lastDirs = append(p.lastDirs, dir)
	p.mu.Unlock()
}

func (p *fakePicker) OpenFiles(dir string) ([]string, error) {
	p.note("open", dir)
	return p.open, p.err
}

func (p *fakePicker) SaveFile(dir, name string) (string, error) {
	p.note("save:"+name, dir)
	if p.err != nil {
		return "", p.err
	}
	return p.save, nil
}

func (p *fakePicker) PickFolder(dir string) (string, error) {
	p.note("folder", dir)
	return p.folder, p.err
}

func (p *fakePicker) callList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// rigDev is the device end of the rig wire. One goroutine reads the wire
// into a queue; each user reads through a view it can detach, so a ZMODEM
// link's reader goroutine left behind after a transfer cannot swallow
// bytes meant for the next reader.
type rigDev struct {
	end *zWireEnd
	in  *zQueue
}

func newRigDev(end *zWireEnd) *rigDev {
	d := &rigDev{end: end, in: newZQueue()}
	go func() {
		buf := make([]byte, 16*1024)
		for {
			n, err := end.Read(buf)
			if n > 0 {
				d.in.push(append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				d.in.close()
				return
			}
		}
	}()
	return d
}

func (d *rigDev) Write(b []byte) (int, error) { return d.end.Write(b) }
func (d *rigDev) Read(b []byte) (int, error)  { return d.in.read(b) }

// view returns a reader/writer whose reads end at detach without taking
// any more bytes from the queue.
func (d *rigDev) view() (*rigView, func()) {
	v := &rigView{d: d}
	return v, func() {
		d.in.mu.Lock()
		v.detached = true
		d.in.mu.Unlock()
		d.in.cond.Broadcast()
	}
}

type rigView struct {
	d        *rigDev
	detached bool
}

func (v *rigView) Write(b []byte) (int, error) { return v.d.end.Write(b) }
func (v *rigView) Read(b []byte) (int, error) {
	q := v.d.in
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.buf) == 0 && !q.closed && !v.detached {
		q.cond.Wait()
	}
	if v.detached || len(q.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(b, q.buf)
	q.buf = q.buf[n:]
	return n, nil
}

type shellRig struct {
	dev    *rigDev
	out    *syncBuffer
	input  *io.PipeWriter
	active []bool
	mu     sync.Mutex
	done   chan error
}

func (r *shellRig) actives() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.active...)
}

// startShellRig runs a session server whose serial port is a wire to a
// fake device, and a sio shell with auto transfer attached to it.
func startShellRig(t *testing.T, picker FilePicker, mutate func(*ShellTransferOptions), wire ...zWireConfig) *shellRig {
	t.Helper()
	cfg := zWireConfig{}
	if len(wire) > 0 {
		cfg = wire[0]
	}
	portEnd, dev := newZWire(cfg, cfg)
	address := freeTCPAddress(t)
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunSessionServer(SessionServerOptions{
			ControlAddress: address, Port: "COM9", Baud: 3000000, Stop: stop,
			OpenPort: func(string, int) (SerialPort, error) { return portEnd, nil },
		})
	}()
	t.Cleanup(func() { close(stop); <-errCh })
	waitForTCPServer(t, address)

	rig := &shellRig{dev: newRigDev(dev), out: &syncBuffer{}, done: make(chan error, 1)}
	inR, inW := io.Pipe()
	rig.input = inW
	opts := &ShellTransferOptions{
		Picker:        picker,
		HandshakeWait: 5 * time.Second,
		Active: func(a bool) {
			rig.mu.Lock()
			rig.active = append(rig.active, a)
			rig.mu.Unlock()
		},
	}
	if mutate != nil {
		mutate(opts)
	}
	go func() {
		rig.done <- StreamShell(ShellStreamOptions{Address: address, Input: inR, Output: rig.out, Transfer: opts})
	}()
	t.Cleanup(func() { _ = inW.Close() })
	// Let the shell connect before the device talks.
	time.Sleep(100 * time.Millisecond)
	return rig
}

func (r *shellRig) deviceWrite(s string) {
	_, _ = r.dev.Write([]byte(s))
}

// deviceTransfer runs the device side of a ZMODEM session on the wire.
func (r *shellRig) deviceTransfer(opts ZModemOptions) (ZModemResult, error) {
	var res ZModemResult
	view, detach := r.dev.view()
	defer detach()
	opts.Link = view
	opts.Result = &res
	if opts.HandshakeWait == 0 {
		opts.HandshakeWait = 10 * time.Second
	}
	opts.DataWait = 3 * time.Second
	// Firmware prints its result line at once; it does not wait for text.
	opts.noDrain = true
	err := TransferZModem(opts)
	return res, err
}

func TestShellAutoReceivesWhenDeviceSends(t *testing.T) {
	dir := t.TempDir()
	save := filepath.Join(dir, "got.wav")
	picker := &fakePicker{save: save}
	var savedDir string
	rig := startShellRig(t, picker, func(o *ShellTransferOptions) {
		o.LastDir = func() string { return dir }
		o.SaveLastDir = func(d string) { savedDir = d }
	})
	data := zPattern(300_000, 30)
	rig.deviceWrite("dev$ sz //mnt/rec/a.wav\r\n")
	res, err := rig.deviceTransfer(ZModemOptions{Files: []ZSendFile{{Name: "a.wav", Data: data}}})
	if err != nil {
		t.Fatalf("device send: %v\nshell:\n%s", err, rig.out.String())
	}
	mustEqualFile(t, save, data)
	if res.Outcome != OutcomeOK {
		t.Fatalf("device outcome = %s", res.Outcome)
	}
	if calls := picker.callList(); len(calls) != 1 || calls[0] != "save:a.wav" {
		t.Fatalf("picker calls = %v", calls)
	}
	if savedDir != dir {
		t.Fatalf("remembered dir = %q, want %q", savedDir, dir)
	}
	rig.deviceWrite("ok a.wav 300000\r\ndev$ ")
	waitFor(t, "shell output after the transfer", 3*time.Second, func() bool {
		out := rig.out.String()
		return strings.Count(out, "dev$") >= 2 && strings.Contains(out, "zmodem: ok")
	})
	out := rig.out.String()
	if strings.Contains(out, "\x18B") || strings.Contains(out, "transfer ended") {
		t.Fatalf("protocol bytes or claim notice shown:\n%q", out)
	}
	if !strings.Contains(out, "zmodem: ok") {
		t.Fatalf("no summary line:\n%s", out)
	}
	if a := rig.actives(); len(a) != 2 || !a[0] || a[1] {
		t.Fatalf("active calls = %v", a)
	}
}

func TestShellAutoSendsWhenDeviceReceives(t *testing.T) {
	src, devDir := t.TempDir(), t.TempDir()
	data := zPattern(200_000, 31)
	path := writeTemp(t, src, "push.bin", data)
	picker := &fakePicker{open: []string{path}}
	rig := startShellRig(t, picker, nil)
	rig.deviceWrite("dev$ rz\r\n")
	res, err := rig.deviceTransfer(ZModemOptions{Receive: true, DestDir: devDir})
	if err != nil {
		t.Fatalf("device recv: %v\nshell:\n%s", err, rig.out.String())
	}
	mustEqualFile(t, filepath.Join(devDir, "push.bin"), data)
	if res.Outcome != OutcomeOK {
		t.Fatalf("device outcome = %s", res.Outcome)
	}
	if calls := picker.callList(); len(calls) != 1 || calls[0] != "open" {
		t.Fatalf("picker calls = %v", calls)
	}
}

func TestShellPickerCancelCancelsDeviceAndRestoresShell(t *testing.T) {
	for _, dir := range []string{"device-sends", "device-receives"} {
		t.Run(dir, func(t *testing.T) {
			picker := &fakePicker{err: ErrPickCancelled}
			rig := startShellRig(t, picker, nil)
			var err error
			var res ZModemResult
			if dir == "device-sends" {
				res, err = rig.deviceTransfer(ZModemOptions{Files: []ZSendFile{{Name: "x.bin", Data: []byte("x")}}})
			} else {
				res, err = rig.deviceTransfer(ZModemOptions{Receive: true, DestDir: t.TempDir()})
			}
			if !errors.Is(err, ErrTransferCancelled) || res.Outcome != OutcomeCancelled {
				t.Fatalf("device err=%v outcome=%s, want cancelled\nshell:\n%s", err, res.Outcome, rig.out.String())
			}
			// The shell shows device output again, and keys reach it once
			// the transfer has handed the keyboard back.
			rig.deviceWrite("cancel\r\ndev$ ")
			waitFor(t, "transfer mode to end", 5*time.Second, func() bool {
				a := rig.actives()
				return len(a) >= 2 && !a[len(a)-1]
			})
			deadline := time.Now().Add(3 * time.Second)
			for !strings.Contains(rig.out.String(), "dev$ ") {
				if time.Now().After(deadline) {
					t.Fatalf("device prompt not shown after cancel:\n%q", rig.out.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			_, _ = rig.input.Write([]byte("help\n"))
			got := make(chan []byte, 1)
			go func() {
				var acc []byte
				buf := make([]byte, 64)
				for !bytes.Contains(acc, []byte("help\r\n")) {
					n, err := rig.dev.Read(buf)
					if err != nil {
						break
					}
					acc = append(acc, buf[:n]...)
				}
				got <- acc
			}()
			select {
			case acc := <-got:
				if !bytes.Contains(acc, []byte("help\r\n")) {
					t.Fatalf("device did not get the typed line after cancel: %q", acc)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("device did not get the typed line after cancel")
			}
			if !strings.Contains(rig.out.String(), "zmodem: cancelled") {
				t.Fatalf("no cancelled summary:\n%s", rig.out.String())
			}
		})
	}
}

func TestShellCtrlCCancelsTransferNotShell(t *testing.T) {
	dir := t.TempDir()
	picker := &fakePicker{save: filepath.Join(dir, "slow.bin")}
	rig := startShellRig(t, picker, nil, zWireConfig{rate: 1 << 20})
	data := zText(4_000_000)
	devErr := make(chan error, 1)
	var res ZModemResult
	go func() {
		var err error
		res, err = rig.deviceTransfer(ZModemOptions{Files: []ZSendFile{{Name: "slow.bin", Data: data}}, Window: 4096})
		devErr <- err
	}()
	waitFor(t, "transfer start", 5*time.Second, func() bool {
		a := rig.actives()
		return len(a) > 0 && a[0]
	})
	time.Sleep(100 * time.Millisecond)
	_, _ = rig.input.Write([]byte{0x03})
	select {
	case err := <-devErr:
		if !errors.Is(err, ErrTransferCancelled) || res.Outcome != OutcomeCancelled {
			t.Fatalf("device err=%v outcome=%s", err, res.Outcome)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Ctrl+C did not cancel the transfer")
	}
	select {
	case err := <-rig.done:
		t.Fatalf("shell exited on Ctrl+C: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(dir, "slow.bin.zpart")); err != nil {
		t.Fatalf("partial file should stay as the resume point: %v", err)
	}
}

func TestShellWithoutTransferOptionsShowsHeaderBytes(t *testing.T) {
	portEnd, dev := newZWire(zWireConfig{}, zWireConfig{})
	address := freeTCPAddress(t)
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunSessionServer(SessionServerOptions{ControlAddress: address, Port: "COM9", Baud: 115200, Stop: stop,
			OpenPort: func(string, int) (SerialPort, error) { return portEnd, nil }})
	}()
	t.Cleanup(func() { close(stop); <-errCh })
	waitForTCPServer(t, address)
	out := &syncBuffer{}
	go func() { _ = StreamShell(ShellStreamOptions{Address: address, Output: out}) }()
	time.Sleep(100 * time.Millisecond)
	header := string(encodeHexHeader(zPosHeader(zRQINIT, 0)))
	_, _ = dev.Write([]byte("rz\r" + header))
	waitFor(t, "raw header in the output", 3*time.Second, func() bool {
		return strings.Contains(out.String(), header)
	})
}

func TestInlinePickerReadsTypedPaths(t *testing.T) {
	dir := t.TempDir()
	lines := make(chan string, 2)
	var out bytes.Buffer
	p := &inlinePicker{out: &out, lines: lines, cancel: make(chan struct{})}
	lines <- ""
	got, err := p.SaveFile(dir, "a.wav")
	if err != nil || got != filepath.Join(dir, "a.wav") {
		t.Fatalf("Enter = %q %v", got, err)
	}
	lines <- "-"
	if _, err := p.SaveFile(dir, "a.wav"); !errors.Is(err, ErrPickCancelled) {
		t.Fatalf("- = %v, want cancelled", err)
	}
	lines <- `"x.bin"; y.bin`
	files, err := p.OpenFiles(dir)
	if err != nil || len(files) != 2 || files[0] != filepath.Join(dir, "x.bin") {
		t.Fatalf("open = %v %v", files, err)
	}
}

// With an unavailable dialog the shell asks on its input line instead.
func TestShellFallsBackToInlinePrompt(t *testing.T) {
	dir := t.TempDir()
	rig := startShellRig(t, &fakePicker{err: ErrPickerUnavailable}, func(o *ShellTransferOptions) {
		o.LastDir = func() string { return dir }
	})
	data := zPattern(5000, 32)
	devErr := make(chan error, 1)
	go func() {
		_, err := rig.deviceTransfer(ZModemOptions{Files: []ZSendFile{{Name: "in.bin", Data: data}}})
		devErr <- err
	}()
	waitFor(t, "inline prompt", 5*time.Second, func() bool { return strings.Contains(rig.out.String(), "save as?") })
	_, _ = rig.input.Write([]byte("\n"))
	if err := <-devErr; err != nil {
		t.Fatalf("device: %v\n%s", err, rig.out.String())
	}
	mustEqualFile(t, filepath.Join(dir, "in.bin"), data)
}
