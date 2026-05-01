package serialcmd

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
)

func TestCopyInputToPortWritesUnescapedLines(t *testing.T) {
	input := bytes.NewBufferString("AT\\r\\n\n")
	var port bytes.Buffer

	if err := copyInputToPort(input, &port, "COM3"); err != nil {
		t.Fatalf("copyInputToPort returned error: %v", err)
	}

	if got := port.String(); got != "AT\r\n" {
		t.Fatalf("written data = %q, want %q", got, "AT\r\n")
	}
}

func TestUnescapeSupportsHexAndControlEscapes(t *testing.T) {
	got := unescape(`\x03\cC\x1b\x04`)
	want := string([]byte{0x03, 0x03, 0x1b, 0x04})

	if got != want {
		t.Fatalf("unescape = %q, want %q", got, want)
	}
}

func TestCopyInputToPortRejectsInvalidHexEscape(t *testing.T) {
	input := bytes.NewBufferString("\\x0\n")
	var port bytes.Buffer

	if err := copyInputToPort(input, &port, "COM3"); err == nil {
		t.Fatal("copyInputToPort returned nil, want invalid hex escape error")
	}
}

func TestCopyInputToPortSendsCRLFForPlainEnteredLine(t *testing.T) {
	input := bytes.NewBufferString("help\n")
	var port bytes.Buffer

	if err := copyInputToPort(input, &port, "COM3"); err != nil {
		t.Fatalf("copyInputToPort returned error: %v", err)
	}

	if got := port.String(); got != "help\r\n" {
		t.Fatalf("written data = %q, want %q", got, "help\r\n")
	}
}

func TestCopyInputToPortPreservesRawCarriageReturnAtEOF(t *testing.T) {
	input := bytes.NewBufferString("AT\r")
	var port bytes.Buffer

	if err := copyInputToPort(input, &port, "COM3"); err != nil {
		t.Fatalf("copyInputToPort returned error: %v", err)
	}

	if got := port.String(); got != "AT\r" {
		t.Fatalf("written data = %q, want AT with raw CR", got)
	}
}

func TestCopyInputToPortWritesRawControlByteImmediately(t *testing.T) {
	inputReader, inputWriter := io.Pipe()
	port := newMemorySerialPort()
	errCh := make(chan error, 1)
	go func() {
		errCh <- copyInputToPort(inputReader, port, "COM3")
	}()

	if _, err := inputWriter.Write([]byte{0x03}); err != nil {
		t.Fatalf("input Write returned error: %v", err)
	}
	if got := port.waitWritten(t); got != string([]byte{0x03}) {
		t.Fatalf("written data = %q, want Ctrl+C byte only", got)
	}
	_ = inputWriter.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("copyInputToPort returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("copyInputToPort did not return after input closed")
	}
}

func TestStreamOutputWritesScreenTeeAndCache(t *testing.T) {
	dir := t.TempDir()
	var screen bytes.Buffer
	teePath := filepath.Join(dir, "logs", "serial.log")
	cachePath := filepath.Join(dir, "cache", "cache.log")

	output, closeOutput, err := streamOutput(StreamOptions{
		Output:    &screen,
		TeePath:   teePath,
		CachePath: cachePath,
	})
	if err != nil {
		t.Fatalf("streamOutput returned error: %v", err)
	}

	if _, err := output.Write([]byte("hello")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	closeOutput()

	if got := screen.String(); got != "hello" {
		t.Fatalf("screen output = %q, want hello", got)
	}
	assertFileContent(t, teePath, "hello")
	assertFileContent(t, cachePath, "hello")
}

func TestStreamOutputAppendsTeeFile(t *testing.T) {
	dir := t.TempDir()
	var screen bytes.Buffer
	teePath := filepath.Join(dir, "serial.log")
	if err := os.WriteFile(teePath, []byte("first\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	output, closeOutput, err := streamOutput(StreamOptions{
		Output:  &screen,
		TeePath: teePath,
	})
	if err != nil {
		t.Fatalf("streamOutput returned error: %v", err)
	}
	if _, err := output.Write([]byte("second\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	closeOutput()

	assertFileContent(t, teePath, "first\nsecond\n")
}

func TestAskWritesPayloadAndCopiesLastResponseLinesToOutputAndCache(t *testing.T) {
	port := newMemorySerialPort()
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "cache.log")
	var out bytes.Buffer

	errCh := make(chan error, 1)
	go func() {
		errCh <- Ask(AskOptions{
			Port:      "COM3",
			Baud:      115200,
			Data:      "AT\\r\\n",
			Timeout:   time.Second,
			MaxLines:  2,
			Output:    &out,
			CachePath: cachePath,
			OpenPort: func(portName string, baud int) (SerialPort, error) {
				if portName != "COM3" || baud != 115200 {
					t.Fatalf("open args = %q %d", portName, baud)
				}
				return port, nil
			},
		})
	}()

	if got := port.waitWritten(t); got != "AT\r\n" {
		t.Fatalf("written data = %q, want AT CRLF", got)
	}
	port.injectRead("OK\r\nREADY\r\nEXTRA\r\n")

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Ask returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not return after reaching line limit")
	}
	if got := out.String(); got != "READY\r\nEXTRA\r\n" {
		t.Fatalf("output = %q, want last two lines", got)
	}
	assertFileContent(t, cachePath, "READY\r\nEXTRA\r\n")
}

func TestAskStopsAfterTimeoutWhenLineLimitIsZero(t *testing.T) {
	port := newMemorySerialPort()
	var out bytes.Buffer

	err := Ask(AskOptions{
		Port:     "COM3",
		Baud:     115200,
		Data:     "\\x03",
		Timeout:  20 * time.Millisecond,
		MaxLines: 0,
		Output:   &out,
		OpenPort: func(portName string, baud int) (SerialPort, error) {
			return port, nil
		},
	})
	if err != nil {
		t.Fatalf("Ask returned error: %v", err)
	}
	if got := port.waitWritten(t); got != string([]byte{0x03}) {
		t.Fatalf("written data = %q, want Ctrl+C", got)
	}
}

func TestBridgeTCPMovesDataBetweenClientAndSerialPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("listener Close returned error: %v", err)
	}

	port := newMemorySerialPort()
	errCh := make(chan error, 1)
	go func() {
		errCh <- BridgeTCP(TCPBridgeOptions{
			ListenAddress: address,
			Port:          "COM3",
			Baud:          115200,
			AcceptOne:     true,
			OpenPort: func(portName string, baud int) (SerialPort, error) {
				if portName != "COM3" || baud != 115200 {
					t.Fatalf("open args = %q %d", portName, baud)
				}
				return port, nil
			},
		})
	}()

	client, err := dialTCP(address)
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}
	defer client.Close()

	if _, err := client.Write([]byte("AT\r\n")); err != nil {
		t.Fatalf("client Write returned error: %v", err)
	}
	if got := port.waitWritten(t); got != "AT\r\n" {
		t.Fatalf("serial write = %q, want AT\\r\\n", got)
	}

	port.injectRead("OK\r\n")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("client Read returned error: %v", err)
	}
	if string(buf) != "OK\r\n" {
		t.Fatalf("client read = %q, want OK\\r\\n", string(buf))
	}
	_ = client.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("BridgeTCP returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BridgeTCP did not return after one accepted client")
	}
}

func TestBridgeTCPOpensSerialPortOnceForMultipleClients(t *testing.T) {
	address := freeTCPAddress(t)
	port := newMemorySerialPort()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	openCount := 0
	go func() {
		errCh <- BridgeTCP(TCPBridgeOptions{
			ListenAddress: address,
			Port:          "COM3",
			Baud:          115200,
			Stop:          stop,
			OpenPort: func(portName string, baud int) (SerialPort, error) {
				openCount++
				return port, nil
			},
		})
	}()
	defer stopTCPBridge(t, stop, errCh)

	first, err := dialTCP(address)
	if err != nil {
		t.Fatalf("first Dial returned error: %v", err)
	}
	defer first.Close()
	second, err := dialTCP(address)
	if err != nil {
		t.Fatalf("second Dial returned error: %v", err)
	}
	defer second.Close()

	if openCount != 1 {
		t.Fatalf("OpenPort called %d times, want 1", openCount)
	}
	if _, err := second.Write([]byte("help\r\n")); err != nil {
		t.Fatalf("second Write returned error: %v", err)
	}
	if got := port.waitWritten(t); got != "help\r\n" {
		t.Fatalf("serial write = %q, want help CRLF", got)
	}

	port.injectRead("OK\r\n")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(first, buf); err != nil {
		t.Fatalf("first Read returned error: %v", err)
	}
	if string(buf) != "OK\r\n" {
		t.Fatalf("first read = %q, want OK CRLF", string(buf))
	}
	if _, err := io.ReadFull(second, buf); err != nil {
		t.Fatalf("second Read returned error: %v", err)
	}
	if string(buf) != "OK\r\n" {
		t.Fatalf("second read = %q, want OK CRLF", string(buf))
	}
}

func TestSessionServerWritesClientDataToSerialPort(t *testing.T) {
	address := freeTCPAddress(t)
	port := newMemorySerialPort()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunSessionServer(SessionServerOptions{
			ControlAddress: address,
			Port:           "COM3",
			Baud:           115200,
			Stop:           stop,
			OpenPort: func(portName string, baud int) (SerialPort, error) {
				if portName != "COM3" || baud != 115200 {
					t.Fatalf("open args = %q %d", portName, baud)
				}
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
}

func TestSessionServerCachesAndBroadcastsSerialOutput(t *testing.T) {
	address := freeTCPAddress(t)
	cachePath := filepath.Join(t.TempDir(), "cache.log")
	port := newMemorySerialPort()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunSessionServer(SessionServerOptions{
			ControlAddress: address,
			Port:           "COM3",
			Baud:           115200,
			CachePath:      cachePath,
			Stop:           stop,
			OpenPort: func(portName string, baud int) (SerialPort, error) {
				return port, nil
			},
		})
	}()
	defer stopSessionServer(t, stop, errCh)

	client, err := dialTCP(address)
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}
	defer client.Close()

	port.injectRead("OK\r\n")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("client Read returned error: %v", err)
	}
	if string(buf) != "OK\r\n" {
		t.Fatalf("client read = %q, want OK CRLF", string(buf))
	}
	assertFileContent(t, cachePath, "OK\r\n")
}

func TestShareBridgeKeepsPhysicalPortOpenAndReadsAsyncOutput(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.log")
	physical := newMemorySerialPort()
	hubA := newMemorySerialPort()
	var physicalOpens int
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- ShareBridge(ShareBridgeOptions{
			PhysicalPort: "COM3",
			HubPorts:     []string{"CNCB20"},
			Baud:         115200,
			CachePath:    cachePath,
			Stop:         stop,
			OpenPort: func(portName string, baud int) (SerialPort, error) {
				if portName == "COM3" {
					physicalOpens++
					return physical, nil
				}
				if portName == "CNCB20" {
					return hubA, nil
				}
				t.Fatalf("unexpected port open %s", portName)
				return nil, nil
			},
		})
	}()
	defer stopShareBridge(t, stop, errCh)

	physical.injectRead("BOOT\r\n")
	if got := hubA.waitWritten(t); got != "BOOT\r\n" {
		t.Fatalf("hub write = %q, want BOOT CRLF", got)
	}
	waitForFileContent(t, cachePath, "BOOT\r\n")
	if physicalOpens != 1 {
		t.Fatalf("physical opens = %d, want 1", physicalOpens)
	}
}

func TestShareBridgeWritesPhysicalResponseToHubPortsAndCache(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.log")
	physical := newMemorySerialPort()
	hubA := newMemorySerialPort()
	hubB := newMemorySerialPort()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- ShareBridge(ShareBridgeOptions{
			PhysicalPort: "COM3",
			HubPorts:     []string{"CNCB20", "CNCB21"},
			Baud:         115200,
			CachePath:    cachePath,
			Stop:         stop,
			OpenPort: openMemoryPorts(t, map[string]*memorySerialPort{
				"COM3":   physical,
				"CNCB20": hubA,
				"CNCB21": hubB,
			}),
		})
	}()
	defer stopShareBridge(t, stop, errCh)

	hubA.injectRead("help\r\n")
	if got := physical.waitWritten(t); got != "help\r\n" {
		t.Fatalf("physical write = %q, want help CRLF", got)
	}
	physical.injectRead("BOOT\r\n")
	if got := hubB.waitWritten(t); got != "BOOT\r\n" {
		t.Fatalf("hubB write = %q, want BOOT CRLF", got)
	}
	assertFileContent(t, cachePath, "BOOT\r\n")
}

func TestShareBridgeCachesPhysicalResponseEvenWhenHubWriteBlocks(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.log")
	physical := newMemorySerialPort()
	hubA := newBlockingWriteSerialPort()
	hubB := newMemorySerialPort()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- ShareBridge(ShareBridgeOptions{
			PhysicalPort: "COM3",
			HubPorts:     []string{"CNCB20", "CNCB21"},
			Baud:         115200,
			CachePath:    cachePath,
			Stop:         stop,
			OpenPort: openSerialPorts(t, map[string]SerialPort{
				"COM3":   physical,
				"CNCB20": hubA,
				"CNCB21": hubB,
			}),
		})
	}()
	defer stopShareBridge(t, stop, errCh)

	hubB.injectRead("help\r\n")
	physical.injectRead("A")
	physical.injectRead("B")
	if got := physical.waitWritten(t); got != "help\r\n" {
		t.Fatalf("physical write = %q, want help CRLF", got)
	}
	hubA.waitWriteStarted(t)
	waitForFileContent(t, cachePath, "AB")
}

func TestShareBridgeRoutesHubPortInputOnlyToPhysical(t *testing.T) {
	physical := newMemorySerialPort()
	hubA := newMemorySerialPort()
	hubB := newMemorySerialPort()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- ShareBridge(ShareBridgeOptions{
			PhysicalPort: "COM3",
			HubPorts:     []string{"CNCB20", "CNCB21"},
			Baud:         115200,
			Stop:         stop,
			OpenPort: openMemoryPorts(t, map[string]*memorySerialPort{
				"COM3":   physical,
				"CNCB20": hubA,
				"CNCB21": hubB,
			}),
		})
	}()
	defer stopShareBridge(t, stop, errCh)

	hubA.injectRead("help\r\n")
	physical.injectRead("OK\r\n")
	if got := physical.waitWritten(t); got != "help\r\n" {
		t.Fatalf("physical write = %q, want help CRLF", got)
	}
	if got := hubB.waitWritten(t); got != "OK\r\n" {
		t.Fatalf("hubB write = %q, want OK CRLF", got)
	}
}

func TestShareBridgeRoutesControlClientInputOnlyToPhysical(t *testing.T) {
	address := freeTCPAddress(t)
	physical := newMemorySerialPort()
	hubA := newMemorySerialPort()
	hubB := newMemorySerialPort()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- ShareBridge(ShareBridgeOptions{
			PhysicalPort:   "COM3",
			HubPorts:       []string{"CNCB20", "CNCB21"},
			Baud:           115200,
			ControlAddress: address,
			Stop:           stop,
			OpenPort: openMemoryPorts(t, map[string]*memorySerialPort{
				"COM3":   physical,
				"CNCB20": hubA,
				"CNCB21": hubB,
			}),
		})
	}()
	defer stopShareBridge(t, stop, errCh)
	waitForTCPServer(t, address)

	physical.injectRead("OK\r\n")
	if err := SendToSession(address, "help\\r\\n"); err != nil {
		t.Fatalf("SendToSession returned error: %v", err)
	}
	if got := physical.waitWritten(t); got != "help\r\n" {
		t.Fatalf("physical write = %q, want help CRLF", got)
	}
	if got := hubB.waitWritten(t); got != "OK\r\n" {
		t.Fatalf("hubB write = %q, want OK CRLF", got)
	}
}

func TestShareBridgeListensOnTCPAddress(t *testing.T) {
	address := freeTCPAddress(t)
	physical := newMemorySerialPort()
	hubA := newMemorySerialPort()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- ShareBridge(ShareBridgeOptions{
			PhysicalPort: "COM3",
			HubPorts:     []string{"CNCB20"},
			Baud:         115200,
			TCPAddress:   address,
			Stop:         stop,
			OpenPort: openMemoryPorts(t, map[string]*memorySerialPort{
				"COM3":   physical,
				"CNCB20": hubA,
			}),
		})
	}()
	defer stopShareBridge(t, stop, errCh)
	waitForTCPServer(t, address)

	physical.injectRead("OK\r\n")
	if err := SendToSession(address, "help\\r\\n"); err != nil {
		t.Fatalf("SendToSession returned error: %v", err)
	}
	if got := physical.waitWritten(t); got != "help\r\n" {
		t.Fatalf("physical write = %q, want help CRLF", got)
	}
	if got := hubA.waitWritten(t); got != "OK\r\n" {
		t.Fatalf("hub write = %q, want OK CRLF", got)
	}
}

func TestShareBridgeDoesNotOpenPublicVirtualPorts(t *testing.T) {
	physical := newMemorySerialPort()
	hubA := newMemorySerialPort()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- ShareBridge(ShareBridgeOptions{
			PhysicalPort: "COM3",
			HubPorts:     []string{"CNCB20"},
			Baud:         115200,
			Stop:         stop,
			OpenPort: openMemoryPorts(t, map[string]*memorySerialPort{
				"COM3":   physical,
				"CNCB20": hubA,
			}),
		})
	}()
	defer stopShareBridge(t, stop, errCh)

	hubA.injectRead("help\r\n")
	physical.injectRead("OK\r\n")
	if got := physical.waitWritten(t); got != "help\r\n" {
		t.Fatalf("physical write = %q, want help CRLF", got)
	}
	if got := hubA.waitWritten(t); got != "OK\r\n" {
		t.Fatalf("hubA write = %q, want OK CRLF", got)
	}
}

func TestShareBridgeReturnsErrorWhenEndpointClosesUnexpectedly(t *testing.T) {
	physical := newMemorySerialPort()
	hubA := newMemorySerialPort()
	errCh := make(chan error, 1)
	go func() {
		errCh <- ShareBridge(ShareBridgeOptions{
			PhysicalPort: "COM3",
			HubPorts:     []string{"CNCB20"},
			Baud:         115200,
			OpenPort: openMemoryPorts(t, map[string]*memorySerialPort{
				"COM3":   physical,
				"CNCB20": hubA,
			}),
		})
	}()

	if err := hubA.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("ShareBridge returned nil, want endpoint close error")
		}
		if !strings.Contains(err.Error(), "read serial port CNCB20") {
			t.Fatalf("error = %v, want CNCB20 read error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ShareBridge did not return after endpoint close")
	}
}

func dialTCP(address string) (net.Conn, error) {
	var lastErr error
	for i := 0; i < 20; i++ {
		conn, err := net.Dial("tcp", address)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	return nil, lastErr
}

func freeTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("listener Close returned error: %v", err)
	}
	return address
}

func waitForTCPServer(t *testing.T, address string) {
	t.Helper()
	conn, err := dialTCP(address)
	if err != nil {
		t.Fatalf("server did not start listening: %v", err)
	}
	_ = conn.Close()
}

func stopSessionServer(t *testing.T, stop chan struct{}, errCh chan error) {
	t.Helper()
	close(stop)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("RunSessionServer returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunSessionServer did not stop")
	}
}

func stopShareBridge(t *testing.T, stop chan struct{}, errCh chan error) {
	t.Helper()
	close(stop)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ShareBridge returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ShareBridge did not stop")
	}
}

func stopTCPBridge(t *testing.T, stop chan struct{}, errCh chan error) {
	t.Helper()
	close(stop)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("BridgeTCP returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BridgeTCP did not stop")
	}
}

func openMemoryPorts(t *testing.T, ports map[string]*memorySerialPort) func(string, int) (SerialPort, error) {
	t.Helper()
	serialPorts := make(map[string]SerialPort, len(ports))
	for name, port := range ports {
		serialPorts[name] = port
	}
	return openSerialPorts(t, serialPorts)
}

func openSerialPorts(t *testing.T, ports map[string]SerialPort) func(string, int) (SerialPort, error) {
	t.Helper()
	return func(portName string, baud int) (SerialPort, error) {
		port := ports[portName]
		if port == nil {
			t.Fatalf("unexpected port open %s at %d", portName, baud)
		}
		return port, nil
	}
}

func assertFileContent(t *testing.T, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) returned error: %v", path, err)
	}
	if got := string(data); got != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func waitForFileContent(t *testing.T, path string, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertFileContent(t, path, want)
}

type memorySerialPort struct {
	mu       sync.Mutex
	written  strings.Builder
	writtenC chan struct{}
	readC    chan []byte
	closeC   chan struct{}
	closed   bool
}

func newMemorySerialPort() *memorySerialPort {
	return &memorySerialPort{
		writtenC: make(chan struct{}, 1),
		readC:    make(chan []byte, 4),
		closeC:   make(chan struct{}),
	}
}

func (p *memorySerialPort) Read(buf []byte) (int, error) {
	select {
	case data := <-p.readC:
		return copy(buf, data), nil
	case <-p.closeC:
		return 0, io.EOF
	}
}

func (p *memorySerialPort) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.written.Write(data)
	select {
	case p.writtenC <- struct{}{}:
	default:
	}
	return len(data), nil
}

func (p *memorySerialPort) Close() error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.closeC)
	}
	p.mu.Unlock()
	return nil
}

func (p *memorySerialPort) injectRead(data string) {
	p.readC <- []byte(data)
}

func (p *memorySerialPort) waitWritten(t *testing.T) string {
	t.Helper()
	select {
	case <-p.writtenC:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for serial write")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.written.String()
}

func (p *memorySerialPort) assertNoWrite(t *testing.T) {
	t.Helper()
	select {
	case <-p.writtenC:
		p.mu.Lock()
		written := p.written.String()
		p.mu.Unlock()
		t.Fatalf("unexpected serial write %q", written)
	case <-time.After(50 * time.Millisecond):
	}
}

type blockingWriteSerialPort struct {
	*memorySerialPort
	writeStarted chan struct{}
}

func newBlockingWriteSerialPort() *blockingWriteSerialPort {
	return &blockingWriteSerialPort{
		memorySerialPort: newMemorySerialPort(),
		writeStarted:     make(chan struct{}),
	}
}

func (p *blockingWriteSerialPort) Write(data []byte) (int, error) {
	select {
	case <-p.writeStarted:
	default:
		close(p.writeStarted)
	}
	<-p.closeC
	return 0, io.EOF
}

func (p *blockingWriteSerialPort) waitWriteStarted(t *testing.T) {
	t.Helper()
	select {
	case <-p.writeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for blocked hub write")
	}
}
