package cli_test

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"go-serial-cli/internal/cli"
	"go-serial-cli/internal/serialcmd"
	"go-serial-cli/internal/session"
)

// runShellClear starts a shell whose terminal reports the given geometry,
// sends Ctrl+L and returns everything the shell printed. withRows off means
// the terminal reported no size, so the shell cannot count rows.
func runShellClear(t *testing.T, rows, cursorRow int, withRows bool) string {
	t.Helper()
	var out lockedBuffer
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name:           "dev1",
		Port:           "COM3",
		Baud:           115200,
		Status:         session.StatusConfigured,
		ControlAddress: "127.0.0.1:1",
		WorkerPID:      4242,
	}); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	stdin, stdinWriter := io.Pipe()
	interrupts := make(chan os.Signal, 1)
	deps := cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: interrupts,
		StreamShell: func(opts serialcmd.ShellStreamOptions) error {
			_, err := io.Copy(io.Discard, opts.Input)
			return err
		},
	}
	if withRows {
		deps.TerminalRows = func(io.Writer) (int, int) { return rows, cursorRow }
	}
	app := cli.New(deps)
	done := make(chan error, 1)
	go func() {
		done <- app.Run([]string{"shell", "dev1"}, &out)
	}()

	if _, err := stdinWriter.Write([]byte{0x0c}); err != nil {
		t.Fatal(err)
	}
	want := "\x1b[2J"
	if withRows {
		want = "\x1b[23A"
	}
	waitForShellOutput(t, &out, want)

	_ = stdinWriter.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shell did not exit after stdin closed")
	}
	return out.String()
}

func TestShellClearScrollsTheScreenIntoTheScrollback(t *testing.T) {
	out := runShellClear(t, 24, 5, true)
	// A newline scrolls only from the last row, so the count is the rows
	// below the cursor plus one full screen. The prompt then goes back up
	// to the top row of the blank screen.
	scroll := strings.Repeat("\r\n", 2*24-1-5) + "\x1b[23A"
	if !strings.Contains(out, scroll) {
		t.Fatalf("clear output = %q, want %q", out, scroll)
	}
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x1b[3J") {
		t.Fatalf("clear output = %q, want no screen or scrollback erase", out)
	}
}

func TestShellClearWithoutTerminalRowsKeepsTheScrollback(t *testing.T) {
	out := runShellClear(t, 0, 0, false)
	if !strings.Contains(out, "\x1b[H\x1b[2J") {
		t.Fatalf("clear output = %q, want the visible-screen clear", out)
	}
	if strings.Contains(out, "\x1b[3J") {
		t.Fatalf("clear output = %q, want the scrollback left alone", out)
	}
}
