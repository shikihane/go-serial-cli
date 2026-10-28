package serialcmd

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func (p *memorySerialPort) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// startLeaseTestServer starts a session server whose ports come from a
// factory, so a reopen can be told apart from the original handle.
func startLeaseTestServer(t *testing.T, open func(call int) (SerialPort, error)) (string, func() []*memorySerialPort, <-chan bool) {
	t.Helper()
	address := freeTCPAddress(t)
	var mu sync.Mutex
	var opened []*memorySerialPort
	changes := make(chan bool, 4)
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunSessionServer(SessionServerOptions{
			ControlAddress: address,
			Port:           "COM3",
			Baud:           115200,
			Stop:           stop,
			OpenPort: func(string, int) (SerialPort, error) {
				mu.Lock()
				call := len(opened)
				mu.Unlock()
				port, err := open(call)
				if err != nil {
					return nil, err
				}
				mem := port.(*memorySerialPort)
				mu.Lock()
				opened = append(opened, mem)
				mu.Unlock()
				return mem, nil
			},
			OnPortLeaseChange: func(released bool) {
				changes <- released
			},
		})
	}()
	opens := func() []*memorySerialPort {
		mu.Lock()
		defer mu.Unlock()
		return append([]*memorySerialPort(nil), opened...)
	}
	t.Cleanup(func() { stopSessionServer(t, stop, errCh) })
	waitForTCPServer(t, address)
	return address, opens, changes
}

func readShellBytes(t *testing.T, conn net.Conn, want string) {
	t.Helper()
	buf := make([]byte, len(want))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("shell read: %v", err)
	}
	if string(buf) != want {
		t.Fatalf("shell read = %q, want %q", buf, want)
	}
}

func TestSessionServerPortLeaseClosesAndReopensThePort(t *testing.T) {
	open := func(call int) (SerialPort, error) {
		return newMemorySerialPort(), nil
	}
	address, opens, changes := startLeaseTestServer(t, open)
	ports := opens()
	if len(ports) != 1 {
		t.Fatalf("opened %d ports at startup, want 1", len(ports))
	}
	first := ports[0]

	shell, err := dialTCP(address)
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()

	first.injectRead("boot")
	readShellBytes(t, shell, "boot")

	if err := ReleaseSessionPort(address); err != nil {
		t.Fatalf("ReleaseSessionPort: %v", err)
	}
	readShellBytes(t, shell, "\r\n[sio: COM3 released for another program; Ctrl+R in sio shell to reopen]\r\n")
	if got := <-changes; !got {
		t.Fatal("release was not reported")
	}
	if !first.isClosed() {
		t.Fatal("release left the port open")
	}

	if _, err := shell.Write([]byte("AT\r")); err != nil {
		t.Fatal(err)
	}
	readShellBytes(t, shell, portReleasedNotice)
	first.assertNoWrite(t)

	if err := AcquireSessionPort(address); err != nil {
		t.Fatalf("AcquireSessionPort: %v", err)
	}
	readShellBytes(t, shell, "\r\n[sio: COM3 reopened at 115200 baud]\r\n")
	if got := <-changes; got {
		t.Fatal("reopen was reported as a release")
	}
	ports = opens()
	if len(ports) != 2 {
		t.Fatalf("opened %d ports after reopen, want 2", len(ports))
	}
	reopened := ports[1]

	reopened.injectRead("OK")
	readShellBytes(t, shell, "OK")
	if _, err := shell.Write([]byte("PING\r")); err != nil {
		t.Fatal(err)
	}
	if got := reopened.waitWritten(t); got != "PING\r" {
		t.Fatalf("write after reopen = %q, want %q", got, "PING\r")
	}
	first.assertNoWrite(t)
}

func TestSessionServerRefusesPortReleaseDuringATransfer(t *testing.T) {
	address, port := startClaimTestServer(t)

	transfer, err := dialTCP(address)
	if err != nil {
		t.Fatal(err)
	}
	defer transfer.Close()
	if _, err := transfer.Write([]byte(transferClaimPreamble)); err != nil {
		t.Fatal(err)
	}
	if got := readClaimAnswer(t, transfer); got != transferClaimOK {
		t.Fatalf("claim answer = %q", got)
	}

	err = ReleaseSessionPort(address)
	if err == nil || !strings.Contains(err.Error(), "file transfer is using the port") {
		t.Fatalf("ReleaseSessionPort error = %v, want a transfer refusal", err)
	}
	if _, err := transfer.Write([]byte("X")); err != nil {
		t.Fatal(err)
	}
	if got := port.waitWritten(t); got != "X" {
		t.Fatalf("write after refused release = %q", got)
	}
}

func TestSessionServerPortAcquireReportsAnOpenFailure(t *testing.T) {
	open := func(call int) (SerialPort, error) {
		if call == 0 {
			return newMemorySerialPort(), nil
		}
		return nil, fmt.Errorf("COM3 is busy")
	}
	address, _, _ := startLeaseTestServer(t, open)

	if err := ReleaseSessionPort(address); err != nil {
		t.Fatalf("ReleaseSessionPort: %v", err)
	}
	err := AcquireSessionPort(address)
	if err == nil || !strings.Contains(err.Error(), "COM3 is busy") {
		t.Fatalf("AcquireSessionPort error = %v, want the port open failure", err)
	}
}
