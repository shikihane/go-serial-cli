package cli_test

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"go-serial-cli/internal/cli"
	"go-serial-cli/internal/serialcmd"
	"go-serial-cli/internal/session"
)

// During a transfer every Ctrl+C reaches the transfer as 0x03 and none of
// them leaves the shell; afterwards two presses leave it again.
func TestShellCtrlCDuringTransferDoesNotExit(t *testing.T) {
	for _, via := range []string{"signal", "console-byte"} {
		t.Run(via, func(t *testing.T) {
			var out bytes.Buffer
			interrupts := make(chan os.Signal, 1)
			stdin, stdinWriter := io.Pipe()
			defer stdinWriter.Close()
			store := session.Store{Dir: t.TempDir()}
			if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, ControlAddress: "127.0.0.1:7005"}); err != nil {
				t.Fatal(err)
			}
			press := func() {
				if via == "signal" {
					interrupts <- os.Interrupt
				} else {
					_, _ = stdinWriter.Write([]byte{0x03})
				}
			}
			app := cli.New(cli.AppDeps{
				Store:           store,
				Stdin:           stdin,
				ShellInterrupts: interrupts,
				FilePicker:      func() serialcmd.FilePicker { return nil },
				StreamShell: func(opts serialcmd.ShellStreamOptions) error {
					if opts.Transfer == nil || opts.Transfer.Active == nil {
						t.Fatal("shell did not get transfer mode")
					}
					buf := make([]byte, 1)
					opts.Transfer.Active(true)
					for i := 0; i < 3; i++ {
						press()
						if _, err := io.ReadFull(opts.Input, buf); err != nil || buf[0] != 0x03 {
							t.Fatalf("press %d in transfer: %q %v", i, buf, err)
						}
						time.Sleep(60 * time.Millisecond)
					}
					opts.Transfer.Active(false)
					press()
					if _, err := io.ReadFull(opts.Input, buf); err != nil || buf[0] != 0x03 {
						t.Fatalf("first press after transfer: %q %v", buf, err)
					}
					time.Sleep(60 * time.Millisecond)
					press()
					if _, err := opts.Input.Read(buf); err != io.EOF {
						t.Fatalf("second press after transfer: err = %v, want EOF", err)
					}
					return nil
				},
			})
			if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
				t.Fatalf("Run: %v", err)
			}
			// Leaving transfer mode draws a fresh prompt.
			if strings.Count(out.String(), ">> ") < 2 {
				t.Fatalf("no fresh prompt after the transfer: %q", out.String())
			}
		})
	}
}

// While the transfer panel owns the screen, keys reach the transfer as
// typed with no echo or line editing; afterwards the line editor is back.
func TestShellRawKeysPassKeysWithoutEcho(t *testing.T) {
	var out lockedBuffer
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, ControlAddress: "127.0.0.1:7005"}); err != nil {
		t.Fatal(err)
	}
	var widthOut io.Writer
	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: make(chan os.Signal),
		FilePicker:      func() serialcmd.FilePicker { return nil },
		TerminalWidth: func(w io.Writer) int {
			widthOut = w
			return 77
		},
		StreamShell: func(opts serialcmd.ShellStreamOptions) error {
			tr := opts.Transfer
			if tr == nil || tr.RawKeys == nil || tr.Width == nil {
				t.Fatal("shell did not get the panel hooks")
			}
			if w := tr.Width(); w != 77 {
				t.Fatalf("width = %d", w)
			}
			tr.Active(true)
			tr.RawKeys(true)
			before := out.String()
			_, _ = stdinWriter.Write([]byte("ab\x1b[A\r"))
			buf := make([]byte, 6)
			if _, err := io.ReadFull(opts.Input, buf); err != nil || string(buf) != "ab\x1b[A\r" {
				t.Fatalf("raw keys = %q %v", buf, err)
			}
			if got := out.String(); got != before {
				t.Fatalf("raw keys echoed: %q", strings.TrimPrefix(got, before))
			}
			tr.RawKeys(false)
			tr.Active(false)
			_, _ = stdinWriter.Write([]byte("x\r"))
			buf = make([]byte, 2)
			if _, err := io.ReadFull(opts.Input, buf); err != nil || string(buf) != "x\n" {
				t.Fatalf("line after raw keys = %q %v", buf, err)
			}
			if !strings.Contains(strings.TrimPrefix(out.String(), before), ">> x") {
				t.Fatalf("typed line not echoed after raw keys: %q", out.String())
			}
			return nil
		},
	})
	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if widthOut != io.Writer(&out) {
		t.Fatalf("width asked for %T, want the shell's own output", widthOut)
	}
}

// Without a TerminalWidth dependency the shell gets no width: the panel
// stays off and transfers print plain lines.
func TestShellWithoutTerminalWidthHasNoPanel(t *testing.T) {
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, ControlAddress: "127.0.0.1:7005"}); err != nil {
		t.Fatal(err)
	}
	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: make(chan os.Signal),
		StreamShell: func(opts serialcmd.ShellStreamOptions) error {
			if opts.Transfer == nil || opts.Transfer.Width != nil {
				t.Fatalf("transfer options = %+v", opts.Transfer)
			}
			return nil
		},
	})
	var out bytes.Buffer
	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
