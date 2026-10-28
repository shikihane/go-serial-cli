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

func savedReleasedSession(t *testing.T) (session.Store, session.State) {
	t.Helper()
	store := session.Store{Dir: t.TempDir()}
	state := session.State{
		Name:           "dev1",
		Port:           "COM3",
		Baud:           115200,
		Status:         session.StatusConfigured,
		ControlAddress: "127.0.0.1:1",
		WorkerPID:      4242,
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := store.MarkPortReleased("dev1"); err != nil {
		t.Fatalf("MarkPortReleased returned error: %v", err)
	}
	return store, state
}

func TestStatusReportsAPortReleasedFromShell(t *testing.T) {
	store, _ := savedReleasedSession(t)
	var out bytes.Buffer
	app := cli.New(cli.AppDeps{
		Store:            store,
		IsProcessRunning: func(pid int) bool { return true },
	})

	if err := app.Run([]string{"status", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(out.String(), "port_state: released") {
		t.Fatalf("status = %q, want a released port line", out.String())
	}

	// A marker whose worker is gone says nothing about the port.
	out.Reset()
	app = cli.New(cli.AppDeps{
		Store:            store,
		IsProcessRunning: func(pid int) bool { return false },
	})
	if err := app.Run([]string{"status", "dev1"}, &out); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if strings.Contains(out.String(), "port_state: released") {
		t.Fatalf("status with a stale worker = %q, want no released line", out.String())
	}
}

func TestSendAndTransferRefuseAReleasedPort(t *testing.T) {
	store, _ := savedReleasedSession(t)
	sent := false
	app := cli.New(cli.AppDeps{
		Store:            store,
		IsProcessRunning: func(pid int) bool { return true },
		SendSessionPayload: func(address string, payload []byte) error {
			sent = true
			return nil
		},
		TransferZModem: func(opts serialcmd.ZModemOptions) error {
			t.Fatal("TransferZModem should not run on a released port")
			return nil
		},
	})

	err := app.Run([]string{"send", "dev1", "AT"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "released COM3 for another program") {
		t.Fatalf("send error = %v, want a released port refusal", err)
	}
	if sent {
		t.Fatal("send wrote to the worker while the port was released")
	}

	err = app.Run([]string{"sz", "dev1", "note.bin"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "released COM3 for another program") {
		t.Fatalf("sz error = %v, want a released port refusal", err)
	}
}

func TestSendKeepsWorkingWhenTheReleasedMarkerIsStale(t *testing.T) {
	store, _ := savedReleasedSession(t)
	var got []byte
	app := cli.New(cli.AppDeps{
		Store:            store,
		IsProcessRunning: func(pid int) bool { return false },
		SendSessionPayload: func(address string, payload []byte) error {
			got = append([]byte(nil), payload...)
			return nil
		},
	})

	if err := app.Run([]string{"send", "dev1", "AT"}, io.Discard); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if string(got) != "AT\r\n" {
		t.Fatalf("sent payload = %q, want %q", got, "AT\r\n")
	}
}

func TestShellClearsReleasesAndReopensWithLocalKeys(t *testing.T) {
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
	startWorkerLog(t, store, "worker start mode=session pid=4242 control=127.0.0.1:1 portlease=1")
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	interrupts := make(chan os.Signal, 1)
	forwarded := make(chan []byte, 16)
	releases := make(chan string, 2)
	acquires := make(chan string, 2)
	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: interrupts,
		ReleaseSessionPort: func(address string) error {
			releases <- address
			return nil
		},
		AcquireSessionPort: func(address string) error {
			acquires <- address
			return nil
		},
		StreamShell: func(opts serialcmd.ShellStreamOptions) error {
			for {
				buf := make([]byte, 256)
				n, err := opts.Input.Read(buf)
				if n > 0 {
					forwarded <- append([]byte(nil), buf[:n]...)
				}
				if err != nil {
					return nil
				}
			}
		},
	})
	done := make(chan error, 1)
	go func() {
		done <- app.Run([]string{"shell", "dev1"}, &out)
	}()

	if _, err := stdinWriter.Write([]byte{0x0c}); err != nil {
		t.Fatal(err)
	}
	waitForShellOutput(t, &out, "\x1b[2J")
	expectNoForwardedInput(t, forwarded)

	if _, err := stdinWriter.Write([]byte{0x13}); err != nil {
		t.Fatal(err)
	}
	select {
	case address := <-releases:
		if address != "127.0.0.1:1" {
			t.Fatalf("release address = %q", address)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl+S did not release the port")
	}

	if _, err := stdinWriter.Write([]byte("AT\n")); err != nil {
		t.Fatal(err)
	}
	expectNoForwardedInput(t, forwarded)

	if _, err := stdinWriter.Write([]byte{0x12}); err != nil {
		t.Fatal(err)
	}
	select {
	case address := <-acquires:
		if address != "127.0.0.1:1" {
			t.Fatalf("acquire address = %q", address)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl+R did not reopen the port")
	}

	if _, err := stdinWriter.Write([]byte("AT\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-forwarded:
		if string(got) != "AT\n" {
			t.Fatalf("forwarded after reopen = %q, want %q", got, "AT\n")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("input was not forwarded after the port reopened")
	}

	_ = stdinWriter.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shell did not exit after stdin closed")
	}
}

func waitForShellOutput(t *testing.T, out *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("shell output %q does not contain %q", out.String(), want)
}

func expectNoForwardedInput(t *testing.T, forwarded chan []byte) {
	t.Helper()
	select {
	case got := <-forwarded:
		t.Fatalf("forwarded %q, want nothing", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func startWorkerLog(t *testing.T, store session.Store, line string) {
	t.Helper()
	if err := session.AppendLog(store.WorkerLogPath("dev1"), line); err != nil {
		t.Fatalf("AppendLog returned error: %v", err)
	}
}

func TestShellDoesNotSendLeaseRequestsToAnOlderWorker(t *testing.T) {
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
	startWorkerLog(t, store, "worker start mode=session pid=4242 control=127.0.0.1:1")
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	forwarded := make(chan []byte, 4)
	app := cli.New(cli.AppDeps{
		Store:           store,
		Stdin:           stdin,
		ShellInterrupts: make(chan os.Signal, 1),
		ReleaseSessionPort: func(address string) error {
			t.Fatal("release must not reach a worker that cannot answer it")
			return nil
		},
		AcquireSessionPort: func(address string) error {
			t.Fatal("acquire must not reach a worker that cannot answer it")
			return nil
		},
		StreamShell: func(opts serialcmd.ShellStreamOptions) error {
			for {
				buf := make([]byte, 256)
				n, err := opts.Input.Read(buf)
				if n > 0 {
					forwarded <- append([]byte(nil), buf[:n]...)
				}
				if err != nil {
					return nil
				}
			}
		},
	})
	done := make(chan error, 1)
	go func() {
		done <- app.Run([]string{"shell", "dev1"}, &out)
	}()

	if _, err := stdinWriter.Write([]byte{0x13}); err != nil {
		t.Fatal(err)
	}
	waitForShellOutput(t, &out, "predates port release")
	expectNoForwardedInput(t, forwarded)

	_ = stdinWriter.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shell did not exit after stdin closed")
	}
}
