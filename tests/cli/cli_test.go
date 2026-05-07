package cli_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go-serial-cli/internal/cli"
	"go-serial-cli/internal/serialcmd"
	"go-serial-cli/internal/session"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func wantPairs(t *testing.T, got []cli.VirtualPortPair, want []cli.VirtualPortPair) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs = %#v, want %#v", got, want)
	}
}

func assertTimestampedLines(t *testing.T, got string, wantPayloads []string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != len(wantPayloads) {
		t.Fatalf("line count = %d, want %d in %q", len(lines), len(wantPayloads), got)
	}
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		const timestampLen = len("06-01-02 15:04:05.000")
		if len(line) <= timestampLen || line[timestampLen] != ' ' {
			t.Fatalf("line %q does not contain compact timestamp and payload", line)
		}
		timestamp := line[:timestampLen]
		if _, err := time.ParseInLocation("06-01-02 15:04:05.000", timestamp, time.Local); err != nil {
			t.Fatalf("timestamp %q did not parse: %v", timestamp, err)
		}
		payload := line[timestampLen+1:]
		if payload != wantPayloads[i] {
			t.Fatalf("line payload = %q, want %q", payload, wantPayloads[i])
		}
	}
}

type interruptingReader struct {
	err           error
	allowNextRead chan struct{}
	sentNext      bool
}

func (r *interruptingReader) Read(buf []byte) (int, error) {
	if r.allowNextRead == nil {
		r.allowNextRead = make(chan struct{}, 1)
	}
	if !r.sentNext {
		r.sentNext = true
		return 0, r.err
	}
	<-r.allowNextRead
	copy(buf, []byte("A\r"))
	return 2, nil
}

func readOneByte(input io.Reader, buf []byte) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(input, buf)
		errCh <- err
	}()
	return errCh
}

func TestSkillInstallCommandAcceptsDotSource(t *testing.T) {
	var out bytes.Buffer
	app := cli.New(cli.AppDeps{
		InstallSkill: func(source string, to string) error {
			if source != "." {
				t.Fatalf("source = %q, want .", source)
			}
			if to != "" {
				t.Fatalf("to = %q, want empty", to)
			}
			return nil
		},
	})

	if err := app.Run([]string{"skill", "install", "."}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestSkillInstallCommandDefaultsToBundledSource(t *testing.T) {
	var out bytes.Buffer
	called := false
	app := cli.New(cli.AppDeps{
		InstallSkill: func(source string, to string) error {
			called = true
			if source != "" {
				t.Fatalf("source = %q, want empty for bundled skill", source)
			}
			if to != "" {
				t.Fatalf("to = %q, want empty", to)
			}
			return nil
		},
	})

	if err := app.Run([]string{"skill", "install"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("InstallSkill was not called")
	}
}

func TestSkillInstallCommandAcceptsToAfterSource(t *testing.T) {
	var out bytes.Buffer
	app := cli.New(cli.AppDeps{
		InstallSkill: func(source string, to string) error {
			if source != "." {
				t.Fatalf("source = %q, want .", source)
			}
			if to != "./skills" {
				t.Fatalf("to = %q, want ./skills", to)
			}
			return nil
		},
	})

	if err := app.Run([]string{"skill", "install", ".", "--to", "./skills"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestToolsExtractCommandReportsExternalTools(t *testing.T) {
	var out bytes.Buffer
	app := cli.New(cli.AppDeps{})

	err := app.Run([]string{"tools", "extract", filepath.Join(t.TempDir(), "tools")}, &out)
	if err == nil {
		t.Fatal("Run returned nil, want external tools error")
	}
	if !strings.Contains(err.Error(), "no third-party tools are bundled") {
		t.Fatalf("Run error = %v", err)
	}
}

func TestHelpPrintsVersionSummary(t *testing.T) {
	var out bytes.Buffer
	app := cli.New(cli.AppDeps{
		Version: cli.VersionInfo{
			Version: "1.2.3",
			Commit:  "abc1234",
			BuiltAt: "2026-04-29T06:20:00Z",
		},
	})

	if err := app.Run([]string{"help"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"version: 1.2.3",
		"commit=abc1234",
		"built_at=2026-04-29T06:20:00Z",
		"sio version",
		"sio -v",
		"sio open <session> <port> [-b baud] [--raw]",
		"sio send <session> [--raw] [-x] <data...>",
		"sio send <session> --file <file>",
		"sio send <session> --xfile <file>",
		"sio ask <session> [--raw] [-x] [-T] <data...> [-t seconds] [-l lines]",
		"sio read <session> [-x] [-T] [-n count] [--to file]",
		"sio check <session> [-x] [-n count] [--from offset] [--rewind count] [--to file]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("help output %q does not contain %q", got, want)
		}
	}
}

func TestHelpUsesConfiguredCommandName(t *testing.T) {
	var out bytes.Buffer
	app := cli.New(cli.AppDeps{CommandName: "gs"})

	if err := app.Run([]string{"help"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := out.String()
	for _, want := range []string{"gs - serial CLI", "gs version", "gs skill install"} {
		if !strings.Contains(got, want) {
			t.Fatalf("help output %q does not contain %q", got, want)
		}
	}
}

func TestVersionCommandPrintsBuildDetails(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"-v"}, {"--version"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			app := cli.New(cli.AppDeps{
				Version: cli.VersionInfo{
					Version: "1.2.3",
					Commit:  "abc1234",
					BuiltAt: "2026-04-29T06:20:00Z",
				},
			})

			if err := app.Run(args, &out); err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			got := out.String()
			for _, want := range []string{
				"version: 1.2.3",
				"commit: abc1234",
				"built_at: 2026-04-29T06:20:00Z",
				"go: ",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("version output %q does not contain %q", got, want)
				}
			}
		})
	}
}

func TestTopLevelCommandsAreRecognized(t *testing.T) {
	commands := [][]string{
		{"ports"},
		{"version"},
		{"-v"},
		{"open", "dev1", "COM3", "-b", "115200"},
		{"send", "dev1", "AT\\r\\n"},
		{"ask", "dev1", "AT\\r\\n"},
		{"read", "dev1", "-n", "200"},
		{"check", "dev1", "-n", "200"},
		{"clear", "dev1"},
		{"shell", "dev1"},
		{"tee", "dev1", "serial.log"},
		{"tcp", "dev1", ":7001"},
		{"share", "dev1", "COM20", "COM21"},
		{"pause", "dev1"},
		{"resume", "dev1"},
		{"status", "dev1"},
		{"log", "dev1"},
		{"stop", "dev1"},
		{"rm", "dev1"},
		{"list"},
	}

	for _, args := range commands {
		t.Run(args[0], func(t *testing.T) {
			var out bytes.Buffer
			app := cli.New(cli.AppDeps{})
			if err := app.Run(args, &out); err != nil {
				t.Fatalf("Run(%v) returned error: %v", args, err)
			}
		})
	}
}

func TestShellStreamsCurrentSession(t *testing.T) {
	var out bytes.Buffer
	input := bytes.NewBufferString("AT\\r\\n")
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	called := false
	app := cli.New(cli.AppDeps{
		Store: store,
		Stdin: input,
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			called = true
			if opts.Port != "COM3" {
				t.Fatalf("Port = %q, want COM3", opts.Port)
			}
			if opts.Baud != 115200 {
				t.Fatalf("Baud = %d, want 115200", opts.Baud)
			}
			if opts.Input != input {
				t.Fatal("Input was not passed to stream")
			}
			if opts.Output != &out {
				t.Fatal("Output was not passed to stream")
			}
			if opts.TeePath != "" {
				t.Fatalf("TeePath = %q, want empty", opts.TeePath)
			}
			if opts.CachePath != store.CachePath("dev1") {
				t.Fatalf("CachePath = %q, want %q", opts.CachePath, store.CachePath("dev1"))
			}
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("StreamSerial was not called")
	}
}

func TestShellUsesSessionControlChannelWhenSessionIsShared(t *testing.T) {
	var out bytes.Buffer
	input := bytes.NewBufferString("AT\\r\\n")
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:           "dev1",
		Port:           "COM3",
		Baud:           115200,
		Status:         session.StatusSharing,
		VirtualPorts:   []string{"COM20", "COM21"},
		HubPorts:       []string{"CNCB20", "CNCB21"},
		ControlAddress: "127.0.0.1:7002",
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	called := false
	app := cli.New(cli.AppDeps{
		Store: store,
		Stdin: input,
		StreamSession: func(address string, gotInput io.Reader, output io.Writer) error {
			called = true
			if address != "127.0.0.1:7002" {
				t.Fatalf("address = %q, want 127.0.0.1:7002", address)
			}
			if gotInput != input {
				t.Fatal("Input was not passed to stream")
			}
			if output != &out {
				t.Fatal("Output was not passed to stream")
			}
			return nil
		},
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			t.Fatal("StreamSerial should not open a shared public virtual port")
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("StreamSerial was not called")
	}
}

func TestShellRejectsSharedSessionWithoutControlChannel(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:         "dev1",
		Port:         "COM3",
		Baud:         115200,
		Status:       session.StatusSharing,
		VirtualPorts: []string{"COM20"},
		HubPorts:     []string{"CNCB20"},
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		Stdin: bytes.NewBuffer(nil),
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			t.Fatal("StreamSerial should not open public virtual or physical ports for a shared session without control")
			return nil
		},
	})

	err := app.Run([]string{"shell", "dev1"}, &out)
	if err == nil {
		t.Fatal("Run returned nil, want missing shared control error")
	}
	if !strings.Contains(err.Error(), "shared session control channel is unavailable") {
		t.Fatalf("error = %v, want shared control error", err)
	}
}

func TestShellForwardsFirstInterruptToStreamInput(t *testing.T) {
	var out bytes.Buffer
	interrupts := make(chan os.Signal, 1)
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: interrupts,
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			interrupts <- os.Interrupt
			buf := make([]byte, 1)
			if _, err := io.ReadFull(opts.Input, buf); err != nil {
				t.Fatalf("ReadFull returned error: %v", err)
			}
			if buf[0] != 0x03 {
				t.Fatalf("input byte = 0x%02x, want Ctrl+C byte", buf[0])
			}
			time.Sleep(100 * time.Millisecond)
			interrupts <- os.Interrupt
			if _, err := opts.Input.Read(buf); err != io.EOF {
				t.Fatalf("second interrupt read error = %v, want EOF", err)
			}
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestShellKeepsInputOpenWhenStdinReadIsInterrupted(t *testing.T) {
	var out bytes.Buffer
	interrupts := make(chan os.Signal, 1)
	stdin := &interruptingReader{err: cli.ErrShellInputInterrupted}
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: interrupts,
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			interrupts <- os.Interrupt
			buf := make([]byte, 1)
			if _, err := io.ReadFull(opts.Input, buf); err != nil {
				t.Fatalf("ReadFull returned error: %v", err)
			}
			if buf[0] != 0x03 {
				t.Fatalf("input byte = 0x%02x, want Ctrl+C byte", buf[0])
			}
			select {
			case stdin.allowNextRead <- struct{}{}:
			case <-time.After(2 * time.Second):
				t.Fatal("timed out allowing next stdin read")
			}
			next := make([]byte, 2)
			if _, err := io.ReadFull(opts.Input, next); err != nil {
				t.Fatalf("input closed after interrupted stdin read: %v", err)
			}
			if string(next) != "A\n" {
				t.Fatalf("next input bytes = %q, want A newline", string(next))
			}
			time.Sleep(100 * time.Millisecond)
			interrupts <- os.Interrupt
			if _, err := opts.Input.Read(next); err != io.EOF {
				t.Fatalf("second interrupt read error = %v, want EOF", err)
			}
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestShellTreatsDuplicateInterruptDeliveryAsOneKeyPress(t *testing.T) {
	var out bytes.Buffer
	interrupts := make(chan os.Signal, 2)
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: interrupts,
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			interrupts <- os.Interrupt
			interrupts <- os.Interrupt
			buf := make([]byte, 2)
			if _, err := opts.Input.Read(buf[:1]); err != nil {
				t.Fatalf("first interrupt read returned error: %v", err)
			}
			select {
			case err := <-readOneByte(opts.Input, buf[1:]):
				if err == nil {
					t.Fatalf("duplicate interrupt wrote an extra byte 0x%02x", buf[1])
				}
				if errors.Is(err, io.EOF) {
					t.Fatal("duplicate interrupt closed shell input")
				}
			case <-time.After(100 * time.Millisecond):
			}
			interrupts <- os.Interrupt
			if _, err := opts.Input.Read(buf[:1]); err != io.EOF {
				t.Fatalf("next real interrupt read error = %v, want EOF", err)
			}
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestShellForwardsFirstRawCtrlCByteAndSecondExits(t *testing.T) {
	var out bytes.Buffer
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: make(chan os.Signal),
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			if _, err := stdinWriter.Write([]byte{0x03}); err != nil {
				t.Fatalf("first stdin Write returned error: %v", err)
			}
			buf := make([]byte, 1)
			if _, err := io.ReadFull(opts.Input, buf); err != nil {
				t.Fatalf("first Ctrl+C read returned error: %v", err)
			}
			if buf[0] != 0x03 {
				t.Fatalf("first Ctrl+C byte = 0x%02x, want 0x03", buf[0])
			}
			time.Sleep(100 * time.Millisecond)
			if _, err := stdinWriter.Write([]byte{0x03}); err != nil {
				t.Fatalf("second stdin Write returned error: %v", err)
			}
			if _, err := opts.Input.Read(buf); err != io.EOF {
				t.Fatalf("second Ctrl+C read error = %v, want EOF", err)
			}
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestShellEchoesRawConsoleInput(t *testing.T) {
	var out bytes.Buffer
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: make(chan os.Signal),
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			if _, err := stdinWriter.Write([]byte("AT\r")); err != nil {
				t.Fatalf("stdin Write returned error: %v", err)
			}
			buf := make([]byte, 3)
			if _, err := io.ReadFull(opts.Input, buf); err != nil {
				t.Fatalf("input read returned error: %v", err)
			}
			_ = stdinWriter.Close()
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := out.String(); !strings.Contains(got, ">> AT\r\n") {
		t.Fatalf("echo output = %q, want prompt and AT CRLF", got)
	}
}

func TestShellNormalizesRawConsoleEnterToLineEnding(t *testing.T) {
	var out bytes.Buffer
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: make(chan os.Signal),
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			if _, err := stdinWriter.Write([]byte("AT\r")); err != nil {
				t.Fatalf("stdin Write returned error: %v", err)
			}
			buf := make([]byte, 3)
			if _, err := io.ReadFull(opts.Input, buf); err != nil {
				t.Fatalf("input read returned error: %v", err)
			}
			if string(buf) != "AT\n" {
				t.Fatalf("input bytes = %q, want AT newline", string(buf))
			}
			_ = stdinWriter.Close()
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestShellPrintsPromptForInteractiveInput(t *testing.T) {
	var out bytes.Buffer
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: make(chan os.Signal),
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			_ = stdinWriter.Close()
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := out.String(); !strings.Contains(got, ">> ") {
		t.Fatalf("output = %q, want prompt", got)
	}
}

func TestShellSavesCommittedLinesToSessionHistory(t *testing.T) {
	var out bytes.Buffer
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: make(chan os.Signal),
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			if _, err := stdinWriter.Write([]byte("AT\rATI\r")); err != nil {
				t.Fatalf("stdin Write returned error: %v", err)
			}
			buf := make([]byte, len("AT\nATI\n"))
			if _, err := io.ReadFull(opts.Input, buf); err != nil {
				t.Fatalf("input read returned error: %v", err)
			}
			_ = stdinWriter.Close()
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	data, err := os.ReadFile(store.HistoryPath("dev1"))
	if err != nil {
		t.Fatalf("ReadFile history returned error: %v", err)
	}
	if got := string(data); got != "AT\nATI\n" {
		t.Fatalf("history = %q, want committed lines", got)
	}
}

func TestShellUpAndDownRecallSessionHistory(t *testing.T) {
	var out bytes.Buffer
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(store.SessionDir("dev1"), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.HistoryPath("dev1"), []byte("AT\nATI\n"), 0o644); err != nil {
		t.Fatalf("WriteFile history returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: make(chan os.Signal),
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			if _, err := stdinWriter.Write([]byte("\x1b[A\x1b[A\x1b[B\r")); err != nil {
				t.Fatalf("stdin Write returned error: %v", err)
			}
			buf := make([]byte, len("ATI\n"))
			if _, err := io.ReadFull(opts.Input, buf); err != nil {
				t.Fatalf("input read returned error: %v", err)
			}
			if string(buf) != "ATI\n" {
				t.Fatalf("input = %q, want ATI newline", string(buf))
			}
			_ = stdinWriter.Close()
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestShellRightAcceptsHistorySuggestion(t *testing.T) {
	var out bytes.Buffer
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(store.SessionDir("dev1"), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.HistoryPath("dev1"), []byte("ATI\n"), 0o644); err != nil {
		t.Fatalf("WriteFile history returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: make(chan os.Signal),
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			if _, err := stdinWriter.Write([]byte("AT\x1b[C\r")); err != nil {
				t.Fatalf("stdin Write returned error: %v", err)
			}
			buf := make([]byte, len("ATI\n"))
			if _, err := io.ReadFull(opts.Input, buf); err != nil {
				t.Fatalf("input read returned error: %v", err)
			}
			if string(buf) != "ATI\n" {
				t.Fatalf("input = %q, want accepted suggestion", string(buf))
			}
			_ = stdinWriter.Close()
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "\x1b[90mI\x1b[0m") {
		t.Fatalf("output = %q, want gray suggestion suffix", got)
	}
}

func TestShellRedrawsPromptAfterStreamOutput(t *testing.T) {
	var out lockedBuffer
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: make(chan os.Signal),
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			if _, err := stdinWriter.Write([]byte("fs")); err != nil {
				t.Fatalf("stdin Write returned error: %v", err)
			}
			deadline := time.After(2 * time.Second)
			for !strings.Contains(out.String(), ">> fs") {
				select {
				case <-deadline:
					t.Fatalf("timed out waiting for current input echo; output = %q", out.String())
				default:
					time.Sleep(10 * time.Millisecond)
				}
			}
			if _, err := opts.Output.Write([]byte("SD:/\r\n")); err != nil {
				t.Fatalf("output Write returned error: %v", err)
			}
			_ = stdinWriter.Close()
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "\r\x1b[2KSD:/\r\n\r\x1b[2K>> fs") {
		t.Fatalf("output = %q, want stream output followed by redrawn prompt and current input", got)
	}
}

func TestShellWrapsDefaultOSStdinForInterruptHandling(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store: store,
		Stdin: os.Stdin,
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			if opts.Input == os.Stdin {
				t.Fatal("shell passed raw os.Stdin instead of wrapping it for interrupt handling")
			}
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestShellConfiguresConsoleForDefaultStdin(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	enabled := false
	cleaned := false

	app := cli.New(cli.AppDeps{
		Store: store,
		Stdin: os.Stdin,
		ConfigureShellConsole: func() func() {
			enabled = true
			return func() {
				cleaned = true
			}
		},
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			if !enabled {
				t.Fatal("shell did not configure console before streaming")
			}
			if cleaned {
				t.Fatal("shell restored console before streaming returned")
			}
			return nil
		},
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !cleaned {
		t.Fatal("shell did not restore console")
	}
}

func TestTeeStreamsCurrentSessionToFile(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev2", Port: "COM4", Baud: 9600}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "serial.log")

	app := cli.New(cli.AppDeps{
		Store: store,
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			if opts.Port != "COM4" {
				t.Fatalf("Port = %q, want COM4", opts.Port)
			}
			if opts.Baud != 9600 {
				t.Fatalf("Baud = %d, want 9600", opts.Baud)
			}
			if opts.Input != nil {
				t.Fatal("tee should not pass input to stream")
			}
			if opts.TeePath != logPath {
				t.Fatalf("TeePath = %q, want %q", opts.TeePath, logPath)
			}
			return nil
		},
	})

	if err := app.Run([]string{"tee", "dev2", logPath}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestShellRejectsPausedSession(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, Paused: true}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		Stdin: bytes.NewBuffer(nil),
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			t.Fatal("StreamSerial should not be called")
			return nil
		},
	})

	err := app.Run([]string{"shell", "dev1"}, &out)
	if err == nil {
		t.Fatal("expected paused session error")
	}
	if !errors.Is(err, cli.ErrSessionPaused) {
		t.Fatalf("error = %v, want ErrSessionPaused", err)
	}
	if !strings.Contains(err.Error(), "run sio resume first") {
		t.Fatalf("error = %v, want sio resume guidance", err)
	}
}

func TestPausedSessionErrorUsesConfiguredCommandName(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, Paused: true}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		CommandName: "gs",
		Store:       store,
		SendSerial: func(port string, baud int, data string) error {
			t.Fatal("SendSerial should not be called")
			return nil
		},
	})

	err := app.Run([]string{"send", "dev1", "AT\\r\\n"}, &out)
	if err == nil {
		t.Fatal("expected paused session error")
	}
	if !errors.Is(err, cli.ErrSessionPaused) {
		t.Fatalf("error = %v, want ErrSessionPaused", err)
	}
	if !strings.Contains(err.Error(), "run gs resume first") {
		t.Fatalf("error = %v, want gs resume guidance", err)
	}
}

func TestShellRequiresStreamImplementationWhenSessionExists(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		Stdin: io.NopCloser(bytes.NewBuffer(nil)),
	})

	if err := app.Run([]string{"shell", "dev1"}, &out); err == nil {
		t.Fatal("expected missing stream implementation error")
	}
}

func TestCommandsRequireSessionName(t *testing.T) {
	commands := [][]string{
		{"open", "COM3"},
		{"send", "AT\\r\\n"},
		{"read", "-n", "200"},
		{"check", "-n", "200"},
		{"clear"},
		{"shell"},
		{"tee", "serial.log"},
		{"tcp", ":7001"},
		{"share", "COM20"},
		{"pause"},
		{"resume"},
		{"status"},
		{"stop"},
	}

	for _, args := range commands {
		t.Run(args[0], func(t *testing.T) {
			var out bytes.Buffer
			app := cli.New(cli.AppDeps{})
			if err := app.Run(args, &out); err == nil {
				t.Fatalf("Run(%v) returned nil error", args)
			}
		})
	}
}

func TestOpenStoresNamedSession(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	app := cli.New(cli.AppDeps{
		Store: store,
		ReserveControlAddress: func() (string, error) {
			return "127.0.0.1:7002", nil
		},
		StartWorker: func(name string) (int, error) {
			return 4242, nil
		},
		WaitForControl: func(address string) error {
			return nil
		},
	})

	if err := app.Run([]string{"open", "dev1", "COM3", "-b", "115200"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.Name != "dev1" || got.Port != "COM3" || got.Baud != 115200 {
		t.Fatalf("state = %#v", got)
	}
	if got.RawMode {
		t.Fatalf("RawMode = true, want false by default")
	}
}

func TestOpenRawStoresRawMode(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	app := cli.New(cli.AppDeps{
		Store: store,
		StartWorker: func(name string) (int, error) {
			return 4242, nil
		},
	})

	if err := app.Run([]string{"open", "dev1", "COM3", "--raw"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !got.RawMode {
		t.Fatalf("RawMode = false, want true")
	}
}

func TestOpenStartsSessionWorkerAndRecordsControlAddress(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	var events []string
	app := cli.New(cli.AppDeps{
		Store: store,
		ReserveControlAddress: func() (string, error) {
			events = append(events, "reserve")
			return "127.0.0.1:7002", nil
		},
		StartWorker: func(name string) (int, error) {
			events = append(events, "start")
			if name != "dev1" {
				t.Fatalf("worker session = %q, want dev1", name)
			}
			state, err := store.Load(name)
			if err != nil {
				t.Fatalf("Load in StartWorker returned error: %v", err)
			}
			if state.ControlAddress != "127.0.0.1:7002" || state.Port != "COM3" || state.Baud != 115200 {
				t.Fatalf("worker saw state = %#v", state)
			}
			return 4242, nil
		},
		WaitForControl: func(address string) error {
			events = append(events, "wait")
			if address != "127.0.0.1:7002" {
				t.Fatalf("wait address = %q, want 127.0.0.1:7002", address)
			}
			return nil
		},
	})

	if err := app.Run([]string{"open", "dev1", "COM3", "-b", "115200"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !reflect.DeepEqual(events, []string{"reserve", "start", "wait"}) {
		t.Fatalf("events = %#v", events)
	}
	if got.ControlAddress != "127.0.0.1:7002" || got.WorkerPID != 4242 || got.Status != session.StatusConfigured {
		t.Fatalf("state = %#v", got)
	}
}

func TestOpenStopsExistingLiveResourcesAndStartsSessionWorker(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM4", Baud: 9600, Status: session.StatusTCP, TCPAddress: "127.0.0.1:47017", WorkerPID: 47017}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	var events []string
	app := cli.New(cli.AppDeps{
		Store: store,
		StopProcess: func(pid int) error {
			events = append(events, fmt.Sprintf("stop:%d", pid))
			return nil
		},
		IsProcessRunning: func(pid int) bool {
			return pid == 47017
		},
		ReserveControlAddress: func() (string, error) {
			events = append(events, "reserve")
			return "127.0.0.1:7002", nil
		},
		StartWorker: func(name string) (int, error) {
			events = append(events, "start")
			return 4242, nil
		},
		WaitForControl: func(address string) error {
			events = append(events, "wait")
			return nil
		},
	})

	if err := app.Run([]string{"open", "dev1", "COM3", "-b", "115200"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !reflect.DeepEqual(events, []string{"stop:47017", "reserve", "start", "wait"}) {
		t.Fatalf("events = %#v", events)
	}
	if got.Port != "COM3" || got.Baud != 115200 || got.Status != session.StatusConfigured || got.TCPAddress != "" || got.WorkerPID != 4242 || got.ControlAddress != "127.0.0.1:7002" {
		t.Fatalf("state = %#v", got)
	}
}

func TestOpenReturnsWorkerStartupErrorAndRemovesNewSession(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	workerPID := 10565
	app := cli.New(cli.AppDeps{
		Store: store,
		ReserveControlAddress: func() (string, error) {
			return "127.0.0.1:10565", nil
		},
		StartWorker: func(name string) (int, error) {
			if err := session.AppendLog(store.WorkerLogPath(name), "worker start mode=session pid=10565 control=127.0.0.1:10565"); err != nil {
				t.Fatalf("AppendLog start returned error: %v", err)
			}
			if err := session.AppendLog(store.WorkerLogPath(name), "worker retry error=\"open serial port COM5: Serial port not found\" delay=250ms"); err != nil {
				t.Fatalf("AppendLog retry returned error: %v", err)
			}
			return workerPID, nil
		},
		WaitForControl: func(address string) error {
			return errors.New("wait for session control 127.0.0.1:10565: connection refused")
		},
		IsProcessRunning: func(pid int) bool {
			return pid == workerPID
		},
		StopProcess: func(pid int) error {
			return nil
		},
	})

	err := app.Run([]string{"open", "dev1", "COM5", "-b", "3000000"}, &out)
	if err == nil {
		t.Fatal("expected worker startup error")
	}
	if !strings.Contains(err.Error(), "open serial port COM5: Serial port not found") {
		t.Fatalf("error = %q, want worker retry error", err)
	}
	if _, err := store.Load("dev1"); err == nil {
		t.Fatal("new failed open left a session behind")
	}
}

func TestOpenRestoresPreviousSessionWhenReplacementFails(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	previous := session.State{Name: "dev1", Port: "COM4", Baud: 9600, Status: session.StatusTCP, TCPAddress: ":7001", WorkerPID: 47017}
	if err := store.Save(previous); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	workerPID := 10565
	app := cli.New(cli.AppDeps{
		Store: store,
		StopProcess: func(pid int) error {
			return nil
		},
		IsProcessRunning: func(pid int) bool {
			return pid == 47017 || pid == workerPID
		},
		ReserveControlAddress: func() (string, error) {
			return "127.0.0.1:10565", nil
		},
		StartWorker: func(name string) (int, error) {
			if err := session.AppendLog(store.WorkerLogPath(name), "worker start mode=session pid=10565 control=127.0.0.1:10565"); err != nil {
				t.Fatalf("AppendLog start returned error: %v", err)
			}
			if err := session.AppendLog(store.WorkerLogPath(name), "worker retry error=\"open serial port COM5: Serial port not found\" delay=250ms"); err != nil {
				t.Fatalf("AppendLog retry returned error: %v", err)
			}
			return workerPID, nil
		},
		WaitForControl: func(address string) error {
			return errors.New("wait for session control 127.0.0.1:10565: connection refused")
		},
	})

	err := app.Run([]string{"open", "dev1", "COM5", "-b", "3000000"}, &out)
	if err == nil {
		t.Fatal("expected worker startup error")
	}
	got, loadErr := store.Load("dev1")
	if loadErr != nil {
		t.Fatalf("Load returned error: %v", loadErr)
	}
	if !reflect.DeepEqual(got, previous) {
		t.Fatalf("previous session was not restored: %#v", got)
	}
}

func TestSendUsesNamedSession(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	called := false
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSerialPayload: func(port string, baud int, payload []byte) error {
			called = true
			if port != "COM3" || baud != 115200 || !bytes.Equal(payload, []byte("AT\r\n")) {
				t.Fatalf("send args = %q %d %q", port, baud, string(payload))
			}
			return nil
		},
	})

	if err := app.Run([]string{"send", "dev1", "AT"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("SendSerial was not called")
	}
}

func TestSendRawDoesNotAppendLineEnding(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSerialPayload: func(port string, baud int, payload []byte) error {
			if !bytes.Equal(payload, []byte("AT")) {
				t.Fatalf("payload = %q, want raw AT", string(payload))
			}
			return nil
		},
	})

	if err := app.Run([]string{"send", "dev1", "--raw", "AT"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestSendRawSessionDoesNotAppendLineEnding(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, RawMode: true}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSerialPayload: func(port string, baud int, payload []byte) error {
			if !bytes.Equal(payload, []byte("AT")) {
				t.Fatalf("payload = %q, want raw AT", string(payload))
			}
			return nil
		},
	})

	if err := app.Run([]string{"send", "dev1", "AT"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestSendDoesNotDoubleAppendExplicitLineEnding(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSerialPayload: func(port string, baud int, payload []byte) error {
			if !bytes.Equal(payload, []byte("AT\r\n")) {
				t.Fatalf("payload = %q, want explicit AT CRLF only", string(payload))
			}
			return nil
		},
	})

	if err := app.Run([]string{"send", "dev1", "AT\\r\\n"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestSendHexUsesNamedSession(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSerialPayload: func(port string, baud int, payload []byte) error {
			if port != "COM3" || baud != 115200 {
				t.Fatalf("port/baud = %q/%d", port, baud)
			}
			if !bytes.Equal(payload, []byte{0x01, 0x0a, 0x02, 0x0f, 0xaf}) {
				t.Fatalf("payload = % x", payload)
			}
			return nil
		},
	})
	if err := app.Run([]string{"send", "dev1", "-x", "01", "0a", "02", "0f", "af"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestSendHexAcceptsQuotedPayload(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSerialPayload: func(port string, baud int, payload []byte) error {
			if !bytes.Equal(payload, []byte{0xaa, 0xbb, 0x1c}) {
				t.Fatalf("payload = % x", payload)
			}
			return nil
		},
	})
	if err := app.Run([]string{"send", "dev1", "--hex", "AA BB 1C"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestSendRawFilePreservesBytes(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	path := filepath.Join(t.TempDir(), "payload.bin")
	want := []byte{0x00, '\\', 'r', '\\', 'n', 0xff}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSerialPayload: func(port string, baud int, payload []byte) error {
			if !bytes.Equal(payload, want) {
				t.Fatalf("payload = % x, want % x", payload, want)
			}
			return nil
		},
	})
	if err := app.Run([]string{"send", "dev1", "--file", path}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestSendHexFileParsesPayload(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	path := filepath.Join(t.TempDir(), "payload.hex")
	if err := os.WriteFile(path, []byte("01 0a\n0x02-ff"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSerialPayload: func(port string, baud int, payload []byte) error {
			if !bytes.Equal(payload, []byte{0x01, 0x0a, 0x02, 0xff}) {
				t.Fatalf("payload = % x", payload)
			}
			return nil
		},
	})
	if err := app.Run([]string{"send", "dev1", "--xfile", path}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestSendRejectsConflictingPayloadSources(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{Store: store})
	err := app.Run([]string{"send", "dev1", "-x", "01", "--file", "payload.bin"}, &out)
	if err == nil {
		t.Fatal("Run returned nil, want conflicting payload source error")
	}
}

func TestSendUsesControlChannelWhenSessionIsShared(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:           "dev1",
		Port:           "COM3",
		Baud:           115200,
		Status:         session.StatusSharing,
		VirtualPorts:   []string{"COM20", "COM21"},
		HubPorts:       []string{"CNCB20", "CNCB21"},
		ControlAddress: "127.0.0.1:7002",
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	called := false
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSession: func(address string, data string) error {
			called = true
			if address != "127.0.0.1:7002" || data != "AT\\r\\n" {
				t.Fatalf("send args = %q %q, want control address and AT\\\\r\\\\n", address, data)
			}
			return nil
		},
		SendSerial: func(port string, baud int, data string) error {
			t.Fatal("SendSerial should not open a shared public virtual port")
			return nil
		},
	})

	if err := app.Run([]string{"send", "dev1", "AT\\r\\n"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("SendSerial was not called")
	}
}

func TestSendRejectsSharedSessionWithoutControlChannel(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:         "dev1",
		Port:         "COM3",
		Baud:         115200,
		Status:       session.StatusSharing,
		VirtualPorts: []string{"COM20"},
		HubPorts:     []string{"CNCB20"},
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSerial: func(port string, baud int, data string) error {
			t.Fatal("SendSerial should not open public virtual or physical ports for a shared session without control")
			return nil
		},
	})

	err := app.Run([]string{"send", "dev1", "AT\\r\\n"}, &out)
	if err == nil {
		t.Fatal("Run returned nil, want missing shared control error")
	}
	if !strings.Contains(err.Error(), "shared session control channel is unavailable") {
		t.Fatalf("error = %v, want shared control error", err)
	}
}

func TestSendUsesSessionControlChannelWhenWorkerOwnsPort(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, ControlAddress: "127.0.0.1:7002"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	called := false
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSession: func(address string, data string) error {
			called = true
			if address != "127.0.0.1:7002" || data != "AT\\r\\n" {
				t.Fatalf("send args = %q %q", address, data)
			}
			return nil
		},
		SendSerial: func(port string, baud int, data string) error {
			t.Fatal("SendSerial should not open the physical port when a worker owns it")
			return nil
		},
	})

	if err := app.Run([]string{"send", "dev1", "AT\\r\\n"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("SendSession was not called")
	}
}

func TestSendUsesTCPForwarderWhenTCPWorkerOwnsPort(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusTCP, TCPAddress: ":7001"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	called := false
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSession: func(address string, data string) error {
			called = true
			if address != "127.0.0.1:7001" || data != "help\\r\\n" {
				t.Fatalf("send args = %q %q", address, data)
			}
			return nil
		},
		SendSerial: func(port string, baud int, data string) error {
			t.Fatal("SendSerial should not open the physical port when tcp worker owns it")
			return nil
		},
	})

	if err := app.Run([]string{"send", "dev1", "help\\r\\n"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("SendSession was not called")
	}
}

func TestAskUsesNamedSessionWithDefaults(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	called := false
	app := cli.New(cli.AppDeps{
		Store: store,
		AskSerial: func(opts serialcmd.AskOptions) error {
			called = true
			if opts.Port != "COM3" || opts.Baud != 115200 || !bytes.Equal(opts.Payload, []byte("AT\r\n")) {
				t.Fatalf("ask opts = %#v", opts)
			}
			if opts.Timeout != 500*time.Millisecond {
				t.Fatalf("timeout = %v, want 500ms", opts.Timeout)
			}
			if opts.MaxLines != 50 {
				t.Fatalf("max lines = %d, want 50", opts.MaxLines)
			}
			if opts.Output != &out {
				t.Fatal("ask should write serial response to command output")
			}
			if opts.CachePath != store.CachePath("dev1") {
				t.Fatalf("cache path = %q, want %q", opts.CachePath, store.CachePath("dev1"))
			}
			return nil
		},
	})

	if err := app.Run([]string{"ask", "dev1", "AT"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("AskSerial was not called")
	}
}

func TestAskRawDoesNotAppendLineEnding(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store: store,
		AskSerial: func(opts serialcmd.AskOptions) error {
			if !bytes.Equal(opts.Payload, []byte("AT")) {
				t.Fatalf("payload = %q, want raw AT", string(opts.Payload))
			}
			return nil
		},
	})

	if err := app.Run([]string{"ask", "dev1", "--raw", "AT"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestAskAcceptsTimeoutAndLineLimit(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	app := cli.New(cli.AppDeps{
		Store: store,
		AskSerial: func(opts serialcmd.AskOptions) error {
			if opts.Timeout != 1500*time.Millisecond {
				t.Fatalf("timeout = %v, want 1.5s", opts.Timeout)
			}
			if opts.MaxLines != 3 {
				t.Fatalf("max lines = %d, want 3", opts.MaxLines)
			}
			return nil
		},
	})

	if err := app.Run([]string{"ask", "dev1", "AT\\r\\n", "-t", "1.5", "-l", "3"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestAskHexUsesNamedSessionAndFormatsResponse(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		AskSerial: func(opts serialcmd.AskOptions) error {
			if !bytes.Equal(opts.Payload, []byte{0x01, 0x03, 0x00, 0x00}) {
				t.Fatalf("payload = % x", opts.Payload)
			}
			if !opts.OutputHex {
				t.Fatal("OutputHex = false, want true")
			}
			_, _ = opts.Output.Write([]byte(serialcmd.FormatHexBytes([]byte{0x01, 0x03, 0x02, 0x00, 0x2a})))
			return nil
		},
	})
	if err := app.Run([]string{"ask", "dev1", "-x", "-t", "1", "01", "03", "00", "00"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.String() != "01 03 02 00 2a\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestAskHexRejectsLineLimit(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{Store: store})
	err := app.Run([]string{"ask", "dev1", "-x", "-l", "5", "01"}, &out)
	if err == nil {
		t.Fatal("Run returned nil, want -l rejection in hex mode")
	}
	if !strings.Contains(err.Error(), "ask -l is not supported with -x") {
		t.Fatalf("error = %v", err)
	}
}

func TestAskHexWorkerControlReadsNewCacheAsHex(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, ControlAddress: "127.0.0.1:7002"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSessionPayload: func(address string, payload []byte) error {
			if !bytes.Equal(payload, []byte{0x01, 0x02}) {
				t.Fatalf("payload = % x", payload)
			}
			if err := os.WriteFile(store.CachePath("dev1"), append([]byte("old"), 0x03), 0o644); err != nil {
				return err
			}
			go func() {
				time.Sleep(40 * time.Millisecond)
				_ = os.WriteFile(store.CachePath("dev1"), append([]byte("old"), 0x03, 0x04), 0o644)
			}()
			return nil
		},
	})
	if err := app.Run([]string{"ask", "dev1", "-x", "-t", "0.12", "01", "02"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.String() != "03\n04\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestAskHexWorkerControlShowsChunkTimestamps(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, ControlAddress: "127.0.0.1:7002"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSessionPayload: func(address string, payload []byte) error {
			if err := os.WriteFile(store.CachePath("dev1"), append([]byte("old"), 0x03), 0o644); err != nil {
				return err
			}
			go func() {
				time.Sleep(40 * time.Millisecond)
				_ = os.WriteFile(store.CachePath("dev1"), append([]byte("old"), 0x03, 0x04), 0o644)
			}()
			return nil
		},
	})
	if err := app.Run([]string{"ask", "dev1", "-x", "-T", "-t", "0.12", "01", "02"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	assertTimestampedLines(t, out.String(), []string{"03", "04"})
}

func TestAskUsesWorkerControlChannelAndReadsNewCache(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, ControlAddress: "127.0.0.1:7002"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("old\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSession: func(address string, data string) error {
			if address != "127.0.0.1:7002" || data != "AT\\r\\n" {
				t.Fatalf("send args = %q %q", address, data)
			}
			return os.WriteFile(store.CachePath("dev1"), []byte("old\nOK\r\n"), 0o644)
		},
		AskSerial: func(opts serialcmd.AskOptions) error {
			t.Fatal("AskSerial should not open the physical port when a worker owns it")
			return nil
		},
	})

	if err := app.Run([]string{"ask", "dev1", "AT\\r\\n", "-t", "0.01"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.String() != "OK\r\n" {
		t.Fatalf("output = %q, want worker cache response", out.String())
	}
}

func TestAskWorkerControlChannelReturnsLastLines(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, ControlAddress: "127.0.0.1:7002"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("old\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSession: func(address string, data string) error {
			return os.WriteFile(store.CachePath("dev1"), []byte("old\none\r\ntwo\r\nthree\r\n"), 0o644)
		},
	})

	if err := app.Run([]string{"ask", "dev1", "AT\\r\\n", "-t", "0.01", "-l", "2"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.String() != "two\r\nthree\r\n" {
		t.Fatalf("output = %q, want last two lines", out.String())
	}
}

func TestAskWorkerControlChannelShowsLineTimestamps(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, ControlAddress: "127.0.0.1:7002"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("old\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		SendSession: func(address string, data string) error {
			return os.WriteFile(store.CachePath("dev1"), []byte("old\none\r\ntwo\r\nthree\r\n"), 0o644)
		},
	})

	if err := app.Run([]string{"ask", "dev1", "AT\\r\\n", "-T", "-t", "0.01", "-l", "2"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	assertTimestampedLines(t, out.String(), []string{"two", "three"})
}

func TestReadWritesCacheToFile(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("first\nsecond\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "dump.log")
	app := cli.New(cli.AppDeps{Store: store})

	if err := app.Run([]string{"read", "dev1", "--to", dest}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(data) != "first\nsecond\n" {
		t.Fatalf("dump = %q, want full cache", string(data))
	}
	if strings.Contains(out.String(), "first") {
		t.Fatalf("read --to should not print cache data, output = %q", out.String())
	}
}

func TestReadWritesLastBytesToFile(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "tail.log")
	app := cli.New(cli.AppDeps{Store: store})

	if err := app.Run([]string{"read", "dev1", "-n", "4", "--to", dest}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(data) != "6789" {
		t.Fatalf("tail dump = %q, want 6789", string(data))
	}
}

func TestReadDoesNotAdvanceCheckCursor(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, CheckOffset: 2}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("abcdef"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{Store: store})

	if err := app.Run([]string{"read", "dev1", "-n", "3"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.CheckOffset != 2 {
		t.Fatalf("CheckOffset = %d, want 2", got.CheckOffset)
	}
}

func TestReadShowsLineTimestamps(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("first\nsecond\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{Store: store})
	if err := app.Run([]string{"read", "dev1", "-T"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	assertTimestampedLines(t, out.String(), []string{"first", "second"})
}

func TestReadHexPrintsLastBytesAsHex(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte{0x00, 0x01, 0x0a, 0xff}, 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{Store: store})
	if err := app.Run([]string{"read", "dev1", "-x", "-n", "3"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.String() != "01 0a ff\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestReadHexWritesFormattedOutputToFile(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte{0x01, 0x02}, 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "cache.hex")
	app := cli.New(cli.AppDeps{Store: store})
	if err := app.Run([]string{"read", "dev1", "-x", "--to", dest}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(data) != "01 02\n" {
		t.Fatalf("file = %q", string(data))
	}
}

func TestCheckReadsFromCursorAndAdvances(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("abcdef"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{Store: store})

	if err := app.Run([]string{"check", "dev1", "-n", "3"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.String() != "abc" {
		t.Fatalf("first check output = %q, want abc", out.String())
	}
	out.Reset()
	if err := app.Run([]string{"check", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.String() != "def" {
		t.Fatalf("second check output = %q, want def", out.String())
	}
	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.CheckOffset != 6 {
		t.Fatalf("CheckOffset = %d, want 6", got.CheckOffset)
	}
}

func TestCheckHexReadsFromCursorAndAdvances(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, CheckOffset: 1}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte{0x00, 0x01, 0x0a, 0xff}, 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{Store: store})
	if err := app.Run([]string{"check", "dev1", "-x", "-n", "2"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.String() != "01 0a\n" {
		t.Fatalf("output = %q", out.String())
	}
	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.CheckOffset != 3 {
		t.Fatalf("CheckOffset = %d, want 3", got.CheckOffset)
	}
}

func TestCheckRewindAllowsReadingEarlierData(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, CheckOffset: 6}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("abcdef"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{Store: store})

	if err := app.Run([]string{"check", "dev1", "--rewind", "4", "-n", "2"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.String() != "cd" {
		t.Fatalf("rewind check output = %q, want cd", out.String())
	}
	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.CheckOffset != 4 {
		t.Fatalf("CheckOffset = %d, want 4", got.CheckOffset)
	}
}

func TestCheckFromWritesToFileAndAdvances(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, CheckOffset: 6}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("abcdef"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "check.log")
	app := cli.New(cli.AppDeps{Store: store})

	if err := app.Run([]string{"check", "dev1", "--from", "1", "--to", dest}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(data) != "bcdef" {
		t.Fatalf("check file = %q, want bcdef", string(data))
	}
	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.CheckOffset != 6 {
		t.Fatalf("CheckOffset = %d, want 6", got.CheckOffset)
	}
}

func TestClearResetsCacheIndex(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, CheckOffset: 3}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(store.CachePath("dev1")), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("abcdef"), 0o644); err != nil {
		t.Fatalf("WriteFile cache returned error: %v", err)
	}
	if err := os.WriteFile(store.CacheIndexPath("dev1"), []byte(`{"at":"2026-05-02T12:00:00+08:00","offset":0,"length":6}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile cache index returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{Store: store})

	if err := app.Run([]string{"clear", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	index, err := os.ReadFile(store.CacheIndexPath("dev1"))
	if err != nil {
		t.Fatalf("ReadFile cache index returned error: %v", err)
	}
	if len(index) != 0 {
		t.Fatalf("cache index = %q, want empty", string(index))
	}
}

func TestShareRecordsVirtualPortsForNamedSession(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{Store: store})

	if err := app.Run([]string{"share", "dev1", "COM20", "COM21"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.Status != session.StatusSharing {
		t.Fatalf("Status = %q, want %q", got.Status, session.StatusSharing)
	}
	if len(got.VirtualPorts) != 2 || got.VirtualPorts[0] != "COM20" || got.VirtualPorts[1] != "COM21" {
		t.Fatalf("VirtualPorts = %#v", got.VirtualPorts)
	}
}

func TestShareStartsWorkerAndRecordsPID(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		StartWorker: func(name string) (int, error) {
			if name != "dev1" {
				t.Fatalf("worker session = %q, want dev1", name)
			}
			state, err := store.Load(name)
			if err != nil {
				t.Fatalf("Load in StartWorker returned error: %v", err)
			}
			if state.Status != session.StatusSharing {
				t.Fatalf("worker saw status = %q, want sharing", state.Status)
			}
			if len(state.VirtualPorts) != 1 || state.VirtualPorts[0] != "COM20" {
				t.Fatalf("worker saw virtual ports = %#v", state.VirtualPorts)
			}
			return 4242, nil
		},
	})

	if err := app.Run([]string{"share", "dev1", "COM20"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.WorkerPID != 4242 {
		t.Fatalf("WorkerPID = %d, want 4242", got.WorkerPID)
	}
}

func TestShareReturnsWorkerRetryErrorAndClearsPID(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM5", Baud: 3000000}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	workerPID := 10565
	app := cli.New(cli.AppDeps{
		Store: store,
		CreateVirtualPorts: func(pairs []cli.VirtualPortPair) error {
			return nil
		},
		ReserveControlAddress: func() (string, error) {
			return "127.0.0.1:10565", nil
		},
		StartWorker: func(name string) (int, error) {
			if err := session.AppendLog(store.WorkerLogPath(name), "worker start mode=share pid=10565"); err != nil {
				t.Fatalf("AppendLog start returned error: %v", err)
			}
			if err := session.AppendLog(store.WorkerLogPath(name), "worker retry error=\"open serial port COM5: Serial port not found\" delay=250ms"); err != nil {
				t.Fatalf("AppendLog retry returned error: %v", err)
			}
			return workerPID, nil
		},
		WaitForControl: func(address string) error {
			return errors.New("wait for session control 127.0.0.1:10565: connection refused")
		},
		IsProcessRunning: func(pid int) bool {
			return pid == workerPID
		},
		StopProcess: func(pid int) error {
			return nil
		},
	})

	err := app.Run([]string{"share", "dev1", "COM93", "COM94"}, &out)
	if err == nil {
		t.Fatal("expected worker startup error")
	}
	if !strings.Contains(err.Error(), "open serial port COM5: Serial port not found") {
		t.Fatalf("error = %q, want worker retry error", err)
	}
	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.WorkerPID != 0 {
		t.Fatalf("WorkerPID = %d, want 0", got.WorkerPID)
	}
}

func TestShareReservesControlAddressForWorkerControl(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		ReserveControlAddress: func() (string, error) {
			return "127.0.0.1:7002", nil
		},
	})

	if err := app.Run([]string{"share", "dev1", "COM20", "COM21"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.ControlAddress != "127.0.0.1:7002" {
		t.Fatalf("ControlAddress = %q, want 127.0.0.1:7002", got.ControlAddress)
	}
}

func TestShareReservesControlAddressForSingleVirtualPort(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		ReserveControlAddress: func() (string, error) {
			return "127.0.0.1:7002", nil
		},
	})

	if err := app.Run([]string{"share", "dev1", "COM20"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.ControlAddress != "127.0.0.1:7002" {
		t.Fatalf("ControlAddress = %q, want 127.0.0.1:7002", got.ControlAddress)
	}
}

func TestShareCreatesVirtualPortPairsBeforeStartingWorker(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	created := false
	app := cli.New(cli.AppDeps{
		Store: store,
		CreateVirtualPorts: func(pairs []cli.VirtualPortPair) error {
			created = true
			wantPairs(t, pairs, []cli.VirtualPortPair{
				{Public: "COM20", Hub: "CNCB20"},
				{Public: "COM21", Hub: "CNCB21"},
			})
			return nil
		},
		StartWorker: func(name string) (int, error) {
			if !created {
				t.Fatal("worker started before virtual port creation")
			}
			return 4242, nil
		},
	})

	if err := app.Run([]string{"share", "dev1", "COM20", "COM21"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestShareRecordsVirtualPortsBeforeCreationFailure(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		CreateVirtualPorts: func(pairs []cli.VirtualPortPair) error {
			return errors.New("COM20 already exists")
		},
		StartWorker: func(name string) (int, error) {
			t.Fatal("worker should not start when virtual port creation fails")
			return 0, nil
		},
	})

	err := app.Run([]string{"share", "dev1", "COM20"}, &out)
	if err == nil {
		t.Fatal("expected virtual port creation error")
	}
	got, loadErr := store.Load("dev1")
	if loadErr != nil {
		t.Fatalf("Load returned error: %v", loadErr)
	}
	if got.Status != session.StatusSharing || !reflect.DeepEqual(got.VirtualPorts, []string{"COM20"}) || !reflect.DeepEqual(got.HubPorts, []string{"CNCB20"}) {
		t.Fatalf("state did not retain failed share resources: %#v", got)
	}
}

func TestAdminSetupCRunsSetupCAndPauses(t *testing.T) {
	var out bytes.Buffer
	var gotArgs []string
	paused := false
	app := cli.New(cli.AppDeps{
		RunSetupC: func(args []string, output io.Writer) error {
			gotArgs = append([]string(nil), args...)
			_, _ = fmt.Fprintln(output, "setupc output")
			return nil
		},
		AdminPause: func() {
			paused = true
		},
	})

	if err := app.Run([]string{"admin", "setupc", "remove", "20"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !reflect.DeepEqual(gotArgs, []string{"remove", "20"}) {
		t.Fatalf("setupc args = %#v", gotArgs)
	}
	if !paused {
		t.Fatal("admin command did not pause")
	}
	for _, want := range []string{"setupc output", "SUCCESS"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q does not contain %q", out.String(), want)
		}
	}
}

func TestAdminSetupCReportsFailureAndPauses(t *testing.T) {
	var out bytes.Buffer
	paused := false
	app := cli.New(cli.AppDeps{
		RunSetupC: func(args []string, output io.Writer) error {
			return errors.New("setupc failed")
		},
		AdminPause: func() {
			paused = true
		},
	})

	err := app.Run([]string{"admin", "setupc", "install", "20"}, &out)
	if err == nil {
		t.Fatal("Run returned nil, want error")
	}
	if !paused {
		t.Fatal("admin command did not pause")
	}
	for _, want := range []string{"FAILED", "setupc failed"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q does not contain %q", out.String(), want)
		}
	}
}

func TestWorkerShareStreamsSessionToCache(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusSharing}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	called := false
	app := cli.New(cli.AppDeps{
		Store: store,
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			called = true
			if opts.Port != "COM3" {
				t.Fatalf("Port = %q, want COM3", opts.Port)
			}
			if opts.Baud != 115200 {
				t.Fatalf("Baud = %d, want 115200", opts.Baud)
			}
			if opts.Input != nil {
				t.Fatal("worker should not pass input to stream")
			}
			if opts.CachePath != store.CachePath("dev1") {
				t.Fatalf("CachePath = %q, want %q", opts.CachePath, store.CachePath("dev1"))
			}
			if opts.Output == nil {
				t.Fatal("worker should provide a non-nil output")
			}
			return nil
		},
	})

	if err := app.Run([]string{"worker", "share", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("StreamSerial was not called")
	}
}

func TestWorkerAutoRunsSessionServerForOpenSession(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:           "dev1",
		Port:           "COM3",
		Baud:           115200,
		Status:         session.StatusConfigured,
		ControlAddress: "127.0.0.1:7002",
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	called := false
	app := cli.New(cli.AppDeps{
		Store: store,
		RunSessionServer: func(opts serialcmd.SessionServerOptions) error {
			called = true
			if opts.ControlAddress != "127.0.0.1:7002" || opts.Port != "COM3" || opts.Baud != 115200 {
				t.Fatalf("session server opts = %#v", opts)
			}
			if opts.CachePath != store.CachePath("dev1") {
				t.Fatalf("CachePath = %q, want %q", opts.CachePath, store.CachePath("dev1"))
			}
			return nil
		},
	})

	if err := app.Run([]string{"worker", "run", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("RunSessionServer was not called")
	}
}

func TestWorkerShareRetriesTransientStreamErrors(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusSharing}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	var attempts int
	var sleeps int
	app := cli.New(cli.AppDeps{
		Store: store,
		StreamSerial: func(opts serialcmd.StreamOptions) error {
			attempts++
			if attempts < 3 {
				return errors.New("temporary serial error")
			}
			return nil
		},
		RetrySleep: func(delay time.Duration) {
			sleeps++
		},
	})

	if err := app.Run([]string{"worker", "share", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if sleeps != 2 {
		t.Fatalf("sleeps = %d, want 2", sleeps)
	}
}

func TestWorkerShareRequiresBuiltInBridgeForVirtualPorts(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:         "dev1",
		Port:         "COM3",
		Baud:         115200,
		Status:       session.StatusSharing,
		VirtualPorts: []string{"COM20", "COM21"},
		HubPorts:     []string{"CNCB20", "CNCB21"},
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{Store: store})

	err := app.Run([]string{"worker", "share", "dev1"}, &out)
	if err == nil {
		t.Fatal("Run returned nil, want share bridge unavailable error")
	}
	if !strings.Contains(err.Error(), "share bridge is unavailable") {
		t.Fatalf("error = %v, want share bridge unavailable", err)
	}
}

func TestWorkerShareUsesGoBridgeWhenAvailable(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:           "dev1",
		Port:           "COM3",
		Baud:           115200,
		Status:         session.StatusSharing,
		VirtualPorts:   []string{"COM20", "COM21"},
		HubPorts:       []string{"CNCB20", "CNCB21"},
		ControlAddress: "127.0.0.1:7002",
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	called := false
	app := cli.New(cli.AppDeps{
		Store: store,
		RunShareBridge: func(opts serialcmd.ShareBridgeOptions) error {
			called = true
			if opts.PhysicalPort != "COM3" || opts.Baud != 115200 || opts.ControlAddress != "127.0.0.1:7002" {
				t.Fatalf("share bridge opts = %#v", opts)
			}
			if !reflect.DeepEqual(opts.HubPorts, []string{"CNCB20", "CNCB21"}) {
				t.Fatalf("HubPorts = %#v", opts.HubPorts)
			}
			if opts.CachePath != store.CachePath("dev1") {
				t.Fatalf("CachePath = %q, want %q", opts.CachePath, store.CachePath("dev1"))
			}
			return nil
		},
	})

	if err := app.Run([]string{"worker", "share", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("RunShareBridge was not called")
	}
}

func TestWorkerAutoKeepsSharedSessionWhenTCPAddressIsSet(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:         "dev1",
		Port:         "COM3",
		Baud:         115200,
		Status:       session.StatusSharing,
		VirtualPorts: []string{"COM20"},
		HubPorts:     []string{"CNCB20"},
		TCPAddress:   ":7001",
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	called := false
	app := cli.New(cli.AppDeps{
		Store: store,
		RunShareBridge: func(opts serialcmd.ShareBridgeOptions) error {
			called = true
			if opts.ControlAddress != ":7001" {
				t.Fatalf("ControlAddress = %q, want :7001", opts.ControlAddress)
			}
			if opts.OnListening == nil {
				t.Fatal("OnListening is nil")
			}
			opts.OnListening(":7001")
			return nil
		},
		BridgeTCP: func(opts serialcmd.TCPBridgeOptions) error {
			t.Fatal("shared session should not use exclusive TCP bridge")
			return nil
		},
	})

	if err := app.Run([]string{"worker", "run", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("RunShareBridge was not called")
	}
	data, err := os.ReadFile(store.WorkerLogPath("dev1"))
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if got := string(data); !strings.Contains(got, "worker ready listen=:7001") {
		t.Fatalf("worker log %q does not contain ready listen", got)
	}
}

func TestWorkerShareRetriesUnexpectedBridgeExit(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:         "dev1",
		Port:         "COM3",
		Baud:         115200,
		Status:       session.StatusSharing,
		VirtualPorts: []string{"COM20"},
		HubPorts:     []string{"CNCB20"},
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	var starts int
	var sleeps int
	app := cli.New(cli.AppDeps{
		Store: store,
		RunShareBridge: func(opts serialcmd.ShareBridgeOptions) error {
			starts++
			if starts == 1 {
				return errors.New("bridge exited")
			}
			return nil
		},
		RetrySleep: func(delay time.Duration) {
			sleeps++
		},
	})

	if err := app.Run([]string{"worker", "share", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if starts != 2 {
		t.Fatalf("bridge starts = %d, want 2", starts)
	}
	if sleeps != 1 {
		t.Fatalf("sleeps = %d, want 1", sleeps)
	}
}

func TestWorkerShareAppendsLifecycleAndBridgeErrors(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:         "dev1",
		Port:         "COM3",
		Baud:         115200,
		Status:       session.StatusSharing,
		VirtualPorts: []string{"COM20", "COM21"},
		HubPorts:     []string{"CNCB20", "CNCB21"},
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	waitErr := errors.New("bridge exited")
	app := cli.New(cli.AppDeps{
		Store: store,
		RunShareBridge: func(opts serialcmd.ShareBridgeOptions) error {
			return waitErr
		},
	})

	err := app.Run([]string{"worker", "share", "dev1"}, &out)
	if !errors.Is(err, waitErr) {
		t.Fatalf("Run error = %v, want %v", err, waitErr)
	}

	data, err := os.ReadFile(store.WorkerLogPath("dev1"))
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		"worker start mode=share pid=",
		"worker error bridge exited",
		"worker exit",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("worker log %q does not contain %q", got, want)
		}
	}
}

func TestTCPStartsWorkerAndRecordsListenAddress(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		StartWorker: func(name string) (int, error) {
			if name != "dev1" {
				t.Fatalf("worker session = %q, want dev1", name)
			}
			state, err := store.Load(name)
			if err != nil {
				t.Fatalf("Load in StartWorker returned error: %v", err)
			}
			if state.TCPAddress != ":7001" {
				t.Fatalf("TCPAddress = %q, want :7001", state.TCPAddress)
			}
			return 7001, nil
		},
	})

	if err := app.Run([]string{"tcp", "dev1", ":7001"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.WorkerPID != 7001 || got.TCPAddress != ":7001" {
		t.Fatalf("state = %#v", got)
	}
}

func TestTCPUsesDefaultPortForHostOnlyListenAddress(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	original := session.State{Name: "dev1", Port: "COM3", Baud: 115200}
	if err := store.Save(original); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		StartWorker: func(name string) (int, error) {
			if name != "dev1" {
				t.Fatalf("worker session = %q, want dev1", name)
			}
			state, err := store.Load(name)
			if err != nil {
				t.Fatalf("Load in StartWorker returned error: %v", err)
			}
			if state.TCPAddress != "127.0.0.1:47017" {
				t.Fatalf("TCPAddress = %q, want 127.0.0.1:47017", state.TCPAddress)
			}
			return 47017, nil
		},
	})

	err := app.Run([]string{"tcp", "dev1", "127.0.0.1"}, &out)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.WorkerPID != 47017 || got.TCPAddress != "127.0.0.1:47017" {
		t.Fatalf("state = %#v", got)
	}
	if !strings.Contains(out.String(), "tcp forwarding dev1 at 127.0.0.1:47017") {
		t.Fatalf("output = %q, want normalized address", out.String())
	}
}

func TestTCPKeepsExplicitListenPort(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		StartWorker: func(name string) (int, error) {
			state, err := store.Load(name)
			if err != nil {
				t.Fatalf("Load in StartWorker returned error: %v", err)
			}
			if state.TCPAddress != ":7001" {
				t.Fatalf("TCPAddress = %q, want :7001", state.TCPAddress)
			}
			return 7001, nil
		},
	})

	if err := app.Run([]string{"tcp", "dev1", ":7001"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(out.String(), "tcp forwarding dev1 at :7001") {
		t.Fatalf("output = %q, want explicit address", out.String())
	}
}

func TestTCPReturnsWorkerStartupErrorAndClearsPID(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	want, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	workerPID := 47017
	var stoppedPID int
	app := cli.New(cli.AppDeps{
		Store: store,
		StartWorker: func(name string) (int, error) {
			if err := session.AppendLog(store.WorkerLogPath(name), "worker start mode=tcp pid=47017 listen=127.0.0.1:47017"); err != nil {
				t.Fatalf("AppendLog start returned error: %v", err)
			}
			if err := session.AppendLog(store.WorkerLogPath(name), "worker retry error=\"listen tcp 127.0.0.1:47017: bind: Only one usage of each socket address is normally permitted.\" delay=250ms"); err != nil {
				t.Fatalf("AppendLog error returned error: %v", err)
			}
			return workerPID, nil
		},
		IsProcessRunning: func(pid int) bool {
			return pid == workerPID
		},
		StopProcess: func(pid int) error {
			stoppedPID = pid
			return nil
		},
	})

	err = app.Run([]string{"tcp", "dev1", "127.0.0.1"}, &out)
	if err == nil {
		t.Fatal("expected worker startup error")
	}
	if !strings.Contains(err.Error(), "listen tcp 127.0.0.1:47017") {
		t.Fatalf("error = %q, want worker listen error", err)
	}
	if stoppedPID != workerPID {
		t.Fatalf("stoppedPID = %d, want %d", stoppedPID, workerPID)
	}
	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state was modified for occupied address: %#v", got)
	}
}

func TestTCPPreservesShareResources(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:         "dev1",
		Port:         "COM3",
		Baud:         115200,
		Status:       session.StatusSharing,
		VirtualPorts: []string{"COM20"},
		HubPorts:     []string{"CNCB20"},
		WorkerPID:    111,
		HubPID:       112,
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	var events []string
	app := cli.New(cli.AppDeps{
		Store: store,
		IsProcessRunning: func(pid int) bool {
			return true
		},
		StopProcess: func(pid int) error {
			events = append(events, fmt.Sprintf("stop:%d", pid))
			return nil
		},
		RemoveVirtualPorts: func(pairs []cli.VirtualPortPair) error {
			t.Fatalf("tcp should not remove existing virtual ports: %#v", pairs)
			return nil
		},
		StartWorker: func(name string) (int, error) {
			events = append(events, "start")
			state, err := store.Load(name)
			if err != nil {
				t.Fatalf("Load in StartWorker returned error: %v", err)
			}
			if state.WorkerPID != 0 || state.HubPID != 0 || !reflect.DeepEqual(state.VirtualPorts, []string{"COM20"}) || !reflect.DeepEqual(state.HubPorts, []string{"CNCB20"}) {
				t.Fatalf("share resources were not preserved before start: %#v", state)
			}
			if state.Status != session.StatusSharing || state.TCPAddress != ":7001" {
				t.Fatalf("tcp should preserve sharing mode and record listen address before start: %#v", state)
			}
			if err := session.AppendLog(store.WorkerLogPath(name), "worker start mode=share pid=7001"); err != nil {
				t.Fatalf("AppendLog start returned error: %v", err)
			}
			if err := session.AppendLog(store.WorkerLogPath(name), "worker ready listen=:7001"); err != nil {
				t.Fatalf("AppendLog ready returned error: %v", err)
			}
			return 7001, nil
		},
	})

	if err := app.Run([]string{"tcp", "dev1", ":7001"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !reflect.DeepEqual(events, []string{"stop:111", "stop:112", "start"}) {
		t.Fatalf("events = %#v", events)
	}
	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.Status != session.StatusSharing || got.TCPAddress != ":7001" || got.WorkerPID != 7001 || got.HubPID != 0 || !reflect.DeepEqual(got.VirtualPorts, []string{"COM20"}) || !reflect.DeepEqual(got.HubPorts, []string{"CNCB20"}) {
		t.Fatalf("state = %#v", got)
	}
}

func TestSharePreservesTCPAddress(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:       "dev1",
		Port:       "COM3",
		Baud:       115200,
		Status:     session.StatusTCP,
		TCPAddress: ":7001",
		WorkerPID:  111,
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		IsProcessRunning: func(pid int) bool {
			return false
		},
		CreateVirtualPorts: func(pairs []cli.VirtualPortPair) error {
			return nil
		},
	})

	if err := app.Run([]string{"share", "dev1", "COM20"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.Status != session.StatusSharing || got.TCPAddress != ":7001" {
		t.Fatalf("state = %#v, want sharing with existing TCPAddress", got)
	}
}

func TestWorkerTCPRunsBridge(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, TCPAddress: ":7001"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	called := false
	app := cli.New(cli.AppDeps{
		Store: store,
		BridgeTCP: func(opts serialcmd.TCPBridgeOptions) error {
			called = true
			if opts.ListenAddress != ":7001" || opts.Port != "COM3" || opts.Baud != 115200 {
				t.Fatalf("bridge opts = %#v", opts)
			}
			if opts.CachePath != store.CachePath("dev1") {
				t.Fatalf("CachePath = %q, want %q", opts.CachePath, store.CachePath("dev1"))
			}
			return nil
		},
	})

	if err := app.Run([]string{"worker", "tcp", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !called {
		t.Fatal("BridgeTCP was not called")
	}
}

func TestWorkerTCPRejectsInvalidListenAddressWithoutRetry(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, TCPAddress: "bad address"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		BridgeTCP: func(opts serialcmd.TCPBridgeOptions) error {
			t.Fatal("BridgeTCP should not be called for invalid listen address")
			return nil
		},
		RetrySleep: func(delay time.Duration) {
			t.Fatal("RetrySleep should not be called for invalid listen address")
		},
	})

	if err := app.Run([]string{"worker", "tcp", "dev1"}, &out); err == nil {
		t.Fatal("expected invalid listen address error")
	}
}

func TestWorkerTCPAppendsLifecycleLogs(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, TCPAddress: ":7001"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	bridgeErr := errors.New("bridge failed")
	app := cli.New(cli.AppDeps{
		Store: store,
		BridgeTCP: func(opts serialcmd.TCPBridgeOptions) error {
			return bridgeErr
		},
	})

	err := app.Run([]string{"worker", "tcp", "dev1"}, &out)
	if !errors.Is(err, bridgeErr) {
		t.Fatalf("Run error = %v, want %v", err, bridgeErr)
	}

	data, err := os.ReadFile(store.WorkerLogPath("dev1"))
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		"worker start mode=tcp pid=",
		"listen=:7001",
		"worker error bridge failed",
		"worker exit",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("worker log %q does not contain %q", got, want)
		}
	}
}

func TestListShowsNamedSessions(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	for _, state := range []session.State{
		{Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusSharing, VirtualPorts: []string{"COM20", "COM21"}, WorkerPID: 123},
		{Name: "dev2", Port: "COM4", Baud: 9600, Status: session.StatusConfigured},
	} {
		if err := store.Save(state); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
	}
	app := cli.New(cli.AppDeps{Store: store})

	if err := app.Run([]string{"list"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got := out.String()
	for _, want := range []string{"NAME", "dev1", "COM3", "COM20,COM21", "dev2", "COM4"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Fatalf("list output %q does not contain %q", got, want)
		}
	}
}

func TestStatusShowsStaleWorkerWhenPIDIsNotRunning(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusSharing, WorkerPID: 123}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		IsProcessRunning: func(pid int) bool {
			if pid != 123 {
				t.Fatalf("pid = %d, want 123", pid)
			}
			return false
		},
	})

	if err := app.Run([]string{"status", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "worker_state: stale") {
		t.Fatalf("status output %q does not contain worker_state: stale", got)
	}
}

func TestStatusShowsLiveResourceDetailsAndLogPaths(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:       "dev1",
		Port:       "COM3",
		Baud:       115200,
		Status:     session.StatusTCP,
		TCPAddress: ":7001",
		WorkerPID:  123,
		HubPID:     456,
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		IsProcessRunning: func(pid int) bool {
			return pid == 123
		},
	})

	if err := app.Run([]string{"status", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"worker_pid: 123",
		"worker_state: running",
		"tcp: :7001",
		"worker_log: " + store.WorkerLogPath("dev1"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output %q does not contain %q", got, want)
		}
	}
	for _, unwanted := range []string{"hub_pid:", "hub_state:", "hub_log:"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("status output %q should not contain %q", got, unwanted)
		}
	}
}

func TestStatusHidesHubDetailsForConfiguredSession(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:           "dev1",
		Port:           "COM3",
		Baud:           115200,
		Status:         session.StatusConfigured,
		ControlAddress: "127.0.0.1:7002",
		WorkerPID:      123,
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		IsProcessRunning: func(pid int) bool {
			return pid == 123
		},
	})

	if err := app.Run([]string{"status", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got := out.String()
	for _, unwanted := range []string{"hub_state:", "hub_log:"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("status output %q should not contain %q", got, unwanted)
		}
	}
}

func TestStatusShowsWorkerRetryError(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:           "dev1",
		Port:           "COM5",
		Baud:           3000000,
		Status:         session.StatusSharing,
		VirtualPorts:   []string{"COM93", "COM94"},
		HubPorts:       []string{"CNCB93", "CNCB94"},
		ControlAddress: "127.0.0.1:10565",
		WorkerPID:      10565,
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := session.AppendLog(store.WorkerLogPath("dev1"), "worker start mode=share pid=10565"); err != nil {
		t.Fatalf("AppendLog start returned error: %v", err)
	}
	if err := session.AppendLog(store.WorkerLogPath("dev1"), "worker retry error=\"open serial port COM5: Serial port not found\" delay=5s"); err != nil {
		t.Fatalf("AppendLog retry returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		IsProcessRunning: func(pid int) bool {
			return pid == 10565
		},
	})

	if err := app.Run([]string{"status", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(out.String(), "worker_error: open serial port COM5: Serial port not found") {
		t.Fatalf("status output %q should contain worker retry error", out.String())
	}
}

func TestLogPrintsWorkerLog(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := session.AppendLog(store.WorkerLogPath("dev1"), "worker start mode=session pid=123"); err != nil {
		t.Fatalf("AppendLog returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{Store: store})

	if err := app.Run([]string{"log", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := out.String(); !strings.Contains(got, "worker start mode=session pid=123") {
		t.Fatalf("log output %q does not contain worker log line", got)
	}
}

func TestLogReportsMissingLogFile(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{Store: store})

	if err := app.Run([]string{"log", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := out.String(); !strings.Contains(got, "no worker log for dev1") {
		t.Fatalf("log output %q does not explain missing worker log", got)
	}
}

func TestListShowsStaleWorkerMarker(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusSharing, WorkerPID: 123}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		IsProcessRunning: func(pid int) bool {
			return false
		},
	})

	if err := app.Run([]string{"list"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "stale") {
		t.Fatalf("list output %q does not contain stale marker", got)
	}
}

func TestStopStopsOnlyNamedSession(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	for _, state := range []session.State{
		{Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusSharing, VirtualPorts: []string{"COM20"}},
		{Name: "dev2", Port: "COM4", Baud: 9600, Status: session.StatusSharing, VirtualPorts: []string{"COM30"}},
	} {
		if err := store.Save(state); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
	}
	app := cli.New(cli.AppDeps{Store: store})

	if err := app.Run([]string{"stop", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	dev1, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load(dev1) returned error: %v", err)
	}
	dev2, err := store.Load("dev2")
	if err != nil {
		t.Fatalf("Load(dev2) returned error: %v", err)
	}
	if dev1.Status != session.StatusStopped {
		t.Fatalf("dev1 status = %q, want stopped", dev1.Status)
	}
	if dev2.Status != session.StatusSharing {
		t.Fatalf("dev2 status = %q, want sharing", dev2.Status)
	}
}

func TestStopKillsOnlyNamedSessionProcesses(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	for _, state := range []session.State{
		{Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusSharing, WorkerPID: 111, HubPID: 112, VirtualPorts: []string{"COM20"}, HubPorts: []string{"CNCB20"}},
		{Name: "dev2", Port: "COM4", Baud: 9600, Status: session.StatusSharing, WorkerPID: 221, HubPID: 222, VirtualPorts: []string{"COM30"}, HubPorts: []string{"CNCB30"}},
	} {
		if err := store.Save(state); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
	}
	var killed []int
	var removed []cli.VirtualPortPair
	app := cli.New(cli.AppDeps{
		Store: store,
		StopProcess: func(pid int) error {
			killed = append(killed, pid)
			return nil
		},
		RemoveVirtualPorts: func(pairs []cli.VirtualPortPair) error {
			removed = append(removed, pairs...)
			return nil
		},
	})

	if err := app.Run([]string{"stop", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if !reflect.DeepEqual(killed, []int{111, 112}) {
		t.Fatalf("killed PIDs = %#v, want []int{111, 112}", killed)
	}
	wantPairs(t, removed, []cli.VirtualPortPair{{Public: "COM20", Hub: "CNCB20"}})
	dev2, err := store.Load("dev2")
	if err != nil {
		t.Fatalf("Load(dev2) returned error: %v", err)
	}
	if dev2.WorkerPID != 221 || dev2.HubPID != 222 {
		t.Fatalf("dev2 PIDs changed: %#v", dev2)
	}
}

func TestStopWaitsForProcessExitBeforeClearingState(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusTCP, WorkerPID: 111, TCPAddress: ":7001"}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	stopped := false
	postStopChecks := 0
	app := cli.New(cli.AppDeps{
		Store: store,
		StopProcess: func(pid int) error {
			if pid != 111 {
				t.Fatalf("StopProcess pid = %d, want 111", pid)
			}
			stopped = true
			return nil
		},
		IsProcessRunning: func(pid int) bool {
			if pid != 111 {
				t.Fatalf("IsProcessRunning pid = %d, want 111", pid)
			}
			if !stopped {
				return true
			}
			postStopChecks++
			return postStopChecks == 1
		},
		RetrySleep: func(delay time.Duration) {},
	})

	if err := app.Run([]string{"stop", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if postStopChecks < 2 {
		t.Fatalf("post-stop liveness checks = %d, want at least 2", postStopChecks)
	}
}

func TestStopSkipsAlreadyGoneProcessesAndStillClearsState(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:         "dev1",
		Port:         "COM3",
		Baud:         115200,
		Status:       session.StatusSharing,
		WorkerPID:    111,
		HubPID:       112,
		VirtualPorts: []string{"COM20"},
		HubPorts:     []string{"CNCB20"},
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	app := cli.New(cli.AppDeps{
		Store: store,
		IsProcessRunning: func(pid int) bool {
			return false
		},
		StopProcess: func(pid int) error {
			t.Fatalf("StopProcess should not be called for stale pid %d", pid)
			return nil
		},
		RemoveVirtualPorts: func(pairs []cli.VirtualPortPair) error {
			return nil
		},
	})

	if err := app.Run([]string{"stop", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.WorkerPID != 0 || got.HubPID != 0 || got.Status != session.StatusStopped {
		t.Fatalf("state was not cleared: %#v", got)
	}
}

func TestStopRetainsVirtualPortsWhenRemovalFails(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:         "dev1",
		Port:         "COM3",
		Baud:         115200,
		Status:       session.StatusSharing,
		WorkerPID:    111,
		HubPID:       112,
		VirtualPorts: []string{"COM20"},
		HubPorts:     []string{"CNCB20"},
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	removeErr := errors.New("COM20 is in use")
	app := cli.New(cli.AppDeps{
		Store: store,
		StopProcess: func(pid int) error {
			return nil
		},
		RemoveVirtualPorts: func(pairs []cli.VirtualPortPair) error {
			return removeErr
		},
	})

	if err := app.Run([]string{"stop", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got, loadErr := store.Load("dev1")
	if loadErr != nil {
		t.Fatalf("Load returned error: %v", loadErr)
	}
	if got.Status != session.StatusStopped || got.WorkerPID != 0 || got.HubPID != 0 {
		t.Fatalf("live state was not cleared despite cleanup failure: %#v", got)
	}
	if !reflect.DeepEqual(got.VirtualPorts, []string{"COM20"}) || !reflect.DeepEqual(got.HubPorts, []string{"CNCB20"}) {
		t.Fatalf("virtual ports were not retained for retry: %#v", got)
	}
	if !strings.Contains(out.String(), "warning: cleanup for dev1 was incomplete") || !strings.Contains(out.String(), "stopped dev1") {
		t.Fatalf("output = %q, want warning and stopped message", out.String())
	}
}

func TestRMRetainsSessionWhenVirtualPortRemovalFails(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:         "dev1",
		Port:         "COM3",
		Baud:         115200,
		Status:       session.StatusSharing,
		WorkerPID:    111,
		HubPID:       112,
		VirtualPorts: []string{"COM20"},
		HubPorts:     []string{"CNCB20"},
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	removeErr := errors.New("COM20 is in use")
	app := cli.New(cli.AppDeps{
		Store: store,
		StopProcess: func(pid int) error {
			return nil
		},
		RemoveVirtualPorts: func(pairs []cli.VirtualPortPair) error {
			return removeErr
		},
	})

	if err := app.Run([]string{"rm", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got, loadErr := store.Load("dev1")
	if loadErr != nil {
		t.Fatalf("Load returned error: %v", loadErr)
	}
	if got.Status != session.StatusStopped || !reflect.DeepEqual(got.VirtualPorts, []string{"COM20"}) || !reflect.DeepEqual(got.HubPorts, []string{"CNCB20"}) {
		t.Fatalf("session did not retain failed cleanup state: %#v", got)
	}
	if !strings.Contains(out.String(), "warning: cleanup for dev1 was incomplete") || !strings.Contains(out.String(), "not removed") {
		t.Fatalf("output = %q, want warning and not removed message", out.String())
	}
}

func TestClearShareStopsShareSessionsAndClearsGlobalPorts(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	for _, state := range []session.State{
		{Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusSharing, WorkerPID: 111, VirtualPorts: []string{"COM20"}, HubPorts: []string{"CNCB20"}},
		{Name: "dev2", Port: "COM4", Baud: 9600, Status: session.StatusTCP, WorkerPID: 222, TCPAddress: ":7001"},
	} {
		if err := store.Save(state); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
	}
	var stopped []int
	cleared := false
	app := cli.New(cli.AppDeps{
		Store: store,
		StopProcess: func(pid int) error {
			stopped = append(stopped, pid)
			return nil
		},
		ClearSharePorts: func() error {
			cleared = true
			return nil
		},
	})

	if err := app.Run([]string{"clear", "--share"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !reflect.DeepEqual(stopped, []int{111}) {
		t.Fatalf("stopped PIDs = %#v, want []int{111}", stopped)
	}
	if !cleared {
		t.Fatal("ClearSharePorts was not called")
	}
	dev1, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load(dev1) returned error: %v", err)
	}
	dev2, err := store.Load("dev2")
	if err != nil {
		t.Fatalf("Load(dev2) returned error: %v", err)
	}
	if dev1.Status != session.StatusStopped || len(dev1.VirtualPorts) != 0 || dev1.WorkerPID != 0 {
		t.Fatalf("dev1 was not cleared: %#v", dev1)
	}
	if dev2.Status != session.StatusTCP || dev2.WorkerPID != 222 {
		t.Fatalf("dev2 should not be touched: %#v", dev2)
	}
	if !strings.Contains(out.String(), "shared virtual ports cleared") {
		t.Fatalf("output = %q, want clear confirmation", out.String())
	}
}

func TestRMStopsLiveResourcesAndDeletesNamedSession(t *testing.T) {
	var out bytes.Buffer
	store := session.Store{Dir: t.TempDir()}
	for _, state := range []session.State{
		{Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusSharing, WorkerPID: 111, HubPID: 112, VirtualPorts: []string{"COM20"}, HubPorts: []string{"CNCB20"}},
		{Name: "dev2", Port: "COM4", Baud: 9600, Status: session.StatusConfigured},
	} {
		if err := store.Save(state); err != nil {
			t.Fatalf("Save returned error: %v", err)
		}
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("cached"), 0o644); err != nil {
		t.Fatalf("WriteFile cache returned error: %v", err)
	}
	if err := os.WriteFile(store.WorkerLogPath("dev1"), []byte("worker log"), 0o644); err != nil {
		t.Fatalf("WriteFile worker log returned error: %v", err)
	}

	var killed []int
	var removed []cli.VirtualPortPair
	app := cli.New(cli.AppDeps{
		Store: store,
		StopProcess: func(pid int) error {
			killed = append(killed, pid)
			return nil
		},
		RemoveVirtualPorts: func(pairs []cli.VirtualPortPair) error {
			removed = append(removed, pairs...)
			return nil
		},
	})

	if err := app.Run([]string{"rm", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !reflect.DeepEqual(killed, []int{111, 112}) {
		t.Fatalf("killed PIDs = %#v, want []int{111, 112}", killed)
	}
	wantPairs(t, removed, []cli.VirtualPortPair{{Public: "COM20", Hub: "CNCB20"}})
	if _, err := os.Stat(store.SessionDir("dev1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dev1 session dir still exists or stat failed differently: %v", err)
	}
	if _, err := store.Load("dev2"); err != nil {
		t.Fatalf("Load(dev2) returned error: %v", err)
	}
	if !strings.Contains(out.String(), "removed dev1") {
		t.Fatalf("output = %q, want removed message", out.String())
	}
}
