package serialcmd

import (
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForOutput(t *testing.T, buf *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("output = %q, want it to contain %q", buf.String(), want)
}

func TestSessionServerCapturesClientTXWithSource(t *testing.T) {
	dir := t.TempDir()
	txPath := filepath.Join(dir, "tx.log")
	txIndexPath := filepath.Join(dir, "tx.index.jsonl")
	address := freeTCPAddress(t)
	port := newMemorySerialPort()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunSessionServer(SessionServerOptions{
			ControlAddress:   address,
			Port:             "COM3",
			Baud:             115200,
			TxCachePath:      txPath,
			TxCacheIndexPath: txIndexPath,
			Stop:             stop,
			OpenPort: func(portName string, baud int) (SerialPort, error) {
				return port, nil
			},
		})
	}()
	defer stopSessionServer(t, stop, errCh)
	waitForTCPServer(t, address)

	if err := SendToSession(address, "help\\r\\n"); err != nil {
		t.Fatalf("SendToSession returned error: %v", err)
	}
	if got := port.waitWritten(t); got != "help\r\n" {
		t.Fatalf("serial write = %q, want help CRLF", got)
	}
	waitForFileContent(t, txPath, "help\r\n")

	chunks := ReadTimedChunks(txPath, txIndexPath, 0, []byte("help\r\n"))
	if len(chunks) != 1 {
		t.Fatalf("tx chunks = %d, want 1", len(chunks))
	}
	if !strings.HasPrefix(chunks[0].Source, "tcp:") {
		t.Fatalf("Source = %q, want tcp: prefix", chunks[0].Source)
	}
}

func TestStreamShellOverlaysExternalTXAndFiltersOwnSends(t *testing.T) {
	dir := t.TempDir()
	txPath := filepath.Join(dir, "tx.log")
	txIndexPath := filepath.Join(dir, "tx.index.jsonl")
	address := freeTCPAddress(t)
	physical := newMemorySerialPort()
	hubA := newMemorySerialPort()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- ShareBridge(ShareBridgeOptions{
			PhysicalPort:     "COM3",
			HubPorts:         []string{"CNCB20"},
			PublicPorts:      []string{"COM20"},
			Baud:             115200,
			ControlAddress:   address,
			TxCachePath:      txPath,
			TxCacheIndexPath: txIndexPath,
			Stop:             stop,
			OpenPort: openMemoryPorts(t, map[string]*memorySerialPort{
				"COM3":   physical,
				"CNCB20": hubA,
			}),
		})
	}()
	defer stopShareBridge(t, stop, errCh)
	waitForTCPServer(t, address)

	var out syncBuffer
	input, inputWriter := io.Pipe()
	shellErr := make(chan error, 1)
	go func() {
		shellErr <- StreamShell(ShellStreamOptions{
			Address:          address,
			Input:            input,
			Output:           &out,
			TxCachePath:      txPath,
			TxCacheIndexPath: txIndexPath,
		})
	}()

	// The shell's own send must reach the physical port but not the overlay.
	if _, err := inputWriter.Write([]byte("mine\n")); err != nil {
		t.Fatalf("input write returned error: %v", err)
	}
	if got := physical.waitWritten(t); got != "mine\r\n" {
		t.Fatalf("physical write = %q, want mine CRLF", got)
	}

	// An external sender on the hub port must show up as a tagged line.
	hubA.injectRead("AT+VER\r\n")
	waitForOutput(t, &out, "[COM20→] AT+VER")

	if strings.Contains(out.String(), "→] mine") {
		t.Fatalf("output = %q, own send must not be overlaid", out.String())
	}

	_ = inputWriter.Close()
	select {
	case <-shellErr:
	case <-time.After(2 * time.Second):
		t.Fatal("StreamShell did not stop after input close")
	}
}

func TestTxLineAssemblerFlushesPartialLineAfterIdle(t *testing.T) {
	var out syncBuffer
	assembler := &txLineAssembler{source: "COM20"}
	assembler.feed([]byte("partial"), &out)
	if out.String() != "" {
		t.Fatalf("output = %q, want empty before idle flush", out.String())
	}
	assembler.lastFeed = time.Now().Add(-time.Second)
	assembler.flushIfIdle(&out)
	if got := out.String(); got != "[COM20→] partial\n" {
		t.Fatalf("output = %q, want idle-flushed tagged line", got)
	}
}
