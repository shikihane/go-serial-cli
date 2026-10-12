package cli_test

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go-serial-cli/internal/cli"
	"go-serial-cli/internal/serialcmd"
	"go-serial-cli/internal/session"
)

// panelDevice is the device end of a fake serial port. One goroutine
// reads the wire; a ZMODEM session reads it through a view that ends at
// detach, so its leftover reader cannot take bytes meant for later.
type panelDevice struct {
	conn     net.Conn
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte
	all      bytes.Buffer
	detached bool
}

func newPanelDevice(conn net.Conn) *panelDevice {
	d := &panelDevice{conn: conn}
	d.cond = sync.NewCond(&d.mu)
	go func() {
		chunk := make([]byte, 4096)
		for {
			n, err := conn.Read(chunk)
			d.mu.Lock()
			d.buf = append(d.buf, chunk[:n]...)
			if d.detached {
				d.all.Write(chunk[:n])
			}
			d.mu.Unlock()
			d.cond.Broadcast()
			if err != nil {
				return
			}
		}
	}()
	return d
}

// Read and Write make the device a slow ZMODEM link: 1 KB per 5 ms.
func (d *panelDevice) Read(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.buf) == 0 && !d.detached {
		d.cond.Wait()
	}
	if d.detached {
		return 0, io.EOF
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}

func (d *panelDevice) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		k := min(len(p), 1024)
		n, err := d.conn.Write(p[:k])
		written += n
		if err != nil {
			return written, err
		}
		p = p[k:]
		time.Sleep(5 * time.Millisecond)
	}
	return written, nil
}

// detach ends the session's reads; what the shell sends later is kept.
func (d *panelDevice) detach() {
	d.mu.Lock()
	d.detached = true
	d.all.Write(d.buf)
	d.buf = nil
	d.mu.Unlock()
	d.cond.Broadcast()
}

func (d *panelDevice) received() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.all.String()
}

type panelPicker struct{ path string }

func (p panelPicker) OpenFiles(string) ([]string, error)      { return nil, serialcmd.ErrPickCancelled }
func (p panelPicker) SaveFile(string, string) (string, error) { return p.path, nil }
func (p panelPicker) PickFolder(string) (string, error)       { return filepath.Dir(p.path), nil }

func waitForText(t *testing.T, what string, out *lockedBuffer, cond func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := out.String()
		if cond(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s:\n%q", what, s)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sio shell on a terminal: the panel replaces the progress line, the
// summary waits for Enter without echoing or forwarding keys, and the
// shell prompt comes back after it.
func TestShellTransferPanelOnTerminal(t *testing.T) {
	portEnd, devEnd := net.Pipe()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	stop := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_ = serialcmd.RunSessionServer(serialcmd.SessionServerOptions{ControlAddress: address, Port: "COM9", Baud: 115200, Stop: stop,
			OpenPort: func(string, int) (serialcmd.SerialPort, error) { return portEnd, nil }})
	}()
	t.Cleanup(func() { close(stop); _ = devEnd.Close(); <-serverDone })
	deadline := time.Now().Add(3 * time.Second)
	for {
		if conn, err := net.Dial("tcp", address); err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	device := newPanelDevice(devEnd)

	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM9", Baud: 115200, ControlAddress: address}); err != nil {
		t.Fatal(err)
	}
	stdin, typed := io.Pipe()
	defer typed.Close()
	var out lockedBuffer
	dest := filepath.Join(t.TempDir(), "got.bin")
	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: make(chan os.Signal),
		FilePicker:      func() serialcmd.FilePicker { return panelPicker{dest} },
		StreamShell:     serialcmd.StreamShell,
		TerminalWidth:   func(io.Writer) int { return 80 },
	})
	go func() { _ = app.Run([]string{"shell", "dev1"}, &out) }()
	time.Sleep(200 * time.Millisecond)

	data := bytes.Repeat([]byte("0123456789abcdef"), 25_000)
	var res serialcmd.ZModemResult
	if err := serialcmd.TransferZModem(serialcmd.ZModemOptions{Link: device, Files: []serialcmd.ZSendFile{{Name: "a.bin", Data: data}},
		Result: &res, HandshakeWait: 10 * time.Second}); err != nil || res.Outcome != serialcmd.OutcomeOK {
		t.Fatalf("device send: %v %s\n%q", err, res.Outcome, out.String())
	}
	device.detach()
	if got, err := os.ReadFile(dest); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("received file: %d bytes, %v", len(got), err)
	}
	const hint = "Press Enter to return to the shell"
	shown := waitForText(t, "summary hint", &out, func(s string) bool { return strings.Contains(s, hint) })
	if !strings.Contains(shown, "a.bin  [#") || !strings.Contains(shown, "retrans 0  CRC err 0") {
		t.Fatalf("no panel progress rows:\n%q", shown)
	}
	if strings.Contains(shown, "zmodem: recv a.bin") {
		t.Fatalf("old progress line printed under the panel:\n%q", shown)
	}
	if !strings.Contains(shown, " ok  a.bin -> ") {
		t.Fatalf("no summary row:\n%q", shown)
	}

	_, _ = devEnd.Write([]byte("dev$ "))
	_, _ = typed.Write([]byte("zz"))
	time.Sleep(300 * time.Millisecond)
	if got := strings.TrimPrefix(out.String(), shown); strings.Contains(got, "zz") || strings.Contains(got, "dev$") {
		t.Fatalf("echo or device output over the summary: %q", got)
	}
	if strings.Contains(device.received(), "zz") {
		t.Fatalf("keys reached the device before Enter: %q", device.received())
	}

	_, _ = typed.Write([]byte("\r"))
	back := waitForText(t, "shell after Enter", &out, func(s string) bool {
		rest := strings.TrimPrefix(s, shown)
		return strings.Contains(rest, "dev$ ") && strings.HasSuffix(rest, ">> ")
	})
	rest := strings.TrimPrefix(back, shown)
	if i, j := strings.Index(rest, "zmodem: ok"), strings.Index(rest, "dev$ "); i < 0 || j < i {
		t.Fatalf("after Enter want the record, then held device output, then the prompt: %q", rest)
	}

	_, _ = typed.Write([]byte("help\r"))
	deadline = time.Now().Add(3 * time.Second)
	for !strings.Contains(device.received(), "help\r\n") {
		if time.Now().After(deadline) {
			t.Fatalf("typed line did not reach the device: %q", device.received())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(device.received(), "zz") {
		t.Fatalf("keys typed under the summary reached the device: %q", device.received())
	}
}
