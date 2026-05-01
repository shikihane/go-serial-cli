package serialcmd

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.bug.st/serial"
)

func TestManualCom0ComShareBridgeThreeWay(t *testing.T) {
	if os.Getenv("SIO_COM0COM_INTEGRATION") != "1" {
		t.Skip("set SIO_COM0COM_INTEGRATION=1 to run against local com0com ports")
	}

	devicePortName := envOrDefault("SIO_TEST_DEVICE_PORT", "COM93")
	bridgePhysicalPortName := envOrDefault("SIO_TEST_BRIDGE_PHYSICAL_PORT", "CNCB93")
	shareClientPortName := envOrDefault("SIO_TEST_SHARE_CLIENT_PORT", "COM94")
	bridgeHubPortName := envOrDefault("SIO_TEST_BRIDGE_HUB_PORT", "CNCB94")
	tcpAddress := envOrDefault("SIO_TEST_TCP_ADDRESS", "127.0.0.1:7003")
	baud := 115200

	device, err := serial.Open(devicePortName, &serial.Mode{BaudRate: baud})
	if err != nil {
		t.Fatalf("open device side %s: %v", devicePortName, err)
	}
	defer device.Close()
	if err := device.SetReadTimeout(2 * time.Second); err != nil {
		t.Fatalf("set read timeout for %s: %v", devicePortName, err)
	}

	shareClient, err := serial.Open(shareClientPortName, &serial.Mode{BaudRate: baud})
	if err != nil {
		t.Fatalf("open share client side %s: %v", shareClientPortName, err)
	}
	defer shareClient.Close()
	if err := shareClient.SetReadTimeout(2 * time.Second); err != nil {
		t.Fatalf("set read timeout for %s: %v", shareClientPortName, err)
	}

	stop := make(chan struct{})
	ready := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- ShareBridge(ShareBridgeOptions{
			PhysicalPort: bridgePhysicalPortName,
			HubPorts:     []string{bridgeHubPortName},
			Baud:         baud,
			TCPAddress:   tcpAddress,
			CachePath:    filepath.Join(t.TempDir(), "cache.log"),
			Stop:         stop,
			OnListening: func(address string) {
				ready <- address
			},
		})
	}()
	defer func() {
		close(stop)
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("ShareBridge stop returned error: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out stopping ShareBridge")
		}
	}()

	select {
	case <-ready:
	case err := <-errCh:
		t.Fatalf("ShareBridge exited before ready: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ShareBridge listener")
	}

	tcpClient, err := net.DialTimeout("tcp", tcpAddress, 2*time.Second)
	if err != nil {
		t.Fatalf("dial tcp bridge %s: %v", tcpAddress, err)
	}
	defer tcpClient.Close()
	if err := tcpClient.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set tcp read deadline: %v", err)
	}

	writeAll(t, tcpClient, []byte("tcp-to-device\n"))
	readContains(t, device, []byte("tcp-to-device\n"), "device from tcp")

	writeAll(t, shareClient, []byte("share-to-device\n"))
	readContains(t, device, []byte("share-to-device\n"), "device from share")

	writeAll(t, device, []byte("device-to-all\n"))
	readContains(t, shareClient, []byte("device-to-all\n"), "share from device")
	readContains(t, tcpClient, []byte("device-to-all\n"), "tcp from device")
}

func envOrDefault(name string, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}

func writeAll(t *testing.T, writer interface {
	Write([]byte) (int, error)
}, data []byte) {
	t.Helper()
	n, err := writer.Write(data)
	if err != nil {
		t.Fatalf("write %q: %v", data, err)
	}
	if n != len(data) {
		t.Fatalf("write %q wrote %d bytes, want %d", data, n, len(data))
	}
}

func readContains(t *testing.T, reader interface {
	Read([]byte) (int, error)
}, want []byte, label string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var got bytes.Buffer
	buf := make([]byte, 256)
	for time.Now().Before(deadline) {
		n, err := reader.Read(buf)
		if n > 0 {
			got.Write(buf[:n])
			if bytes.Contains(got.Bytes(), want) {
				return
			}
		}
		if err != nil && got.Len() == 0 {
			continue
		}
	}
	t.Fatalf("%s read %q, want to contain %q", label, got.String(), string(want))
}

func TestManualCom0ComShareBridgeDropsUnreadHubWithoutStopping(t *testing.T) {
	if os.Getenv("SIO_COM0COM_INTEGRATION") != "1" {
		t.Skip("set SIO_COM0COM_INTEGRATION=1 to run against local com0com ports")
	}

	devicePortName := envOrDefault("SIO_TEST_DEVICE_PORT", "COM93")
	bridgePhysicalPortName := envOrDefault("SIO_TEST_BRIDGE_PHYSICAL_PORT", "CNCB93")
	bridgeHubPortName := envOrDefault("SIO_TEST_BRIDGE_HUB_PORT", "CNCB94")
	tcpAddress := envOrDefault("SIO_TEST_TCP_ADDRESS", "127.0.0.1:7003")
	baud := 115200

	device, err := serial.Open(devicePortName, &serial.Mode{BaudRate: baud})
	if err != nil {
		t.Fatalf("open device side %s: %v", devicePortName, err)
	}
	defer device.Close()
	if err := device.SetReadTimeout(2 * time.Second); err != nil {
		t.Fatalf("set read timeout for %s: %v", devicePortName, err)
	}

	stop := make(chan struct{})
	stopClosed := false
	closeStop := func() {
		if !stopClosed {
			close(stop)
			stopClosed = true
		}
	}
	ready := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- ShareBridge(ShareBridgeOptions{
			PhysicalPort: bridgePhysicalPortName,
			HubPorts:     []string{bridgeHubPortName},
			Baud:         baud,
			TCPAddress:   tcpAddress,
			CachePath:    filepath.Join(t.TempDir(), "cache.log"),
			Stop:         stop,
			OnListening: func(address string) {
				ready <- address
			},
		})
	}()
	defer closeStop()

	select {
	case <-ready:
	case err := <-errCh:
		t.Fatalf("ShareBridge exited before ready: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ShareBridge listener")
	}

	tcpClient, err := net.DialTimeout("tcp", tcpAddress, 2*time.Second)
	if err != nil {
		t.Fatalf("dial tcp bridge %s: %v", tcpAddress, err)
	}
	defer tcpClient.Close()
	if err := tcpClient.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set tcp read deadline: %v", err)
	}

	writeAll(t, device, []byte("first-unread-hub-output\n"))
	time.Sleep(2 * shareEndpointWriteTimeout)
	select {
	case err := <-errCh:
		t.Fatalf("ShareBridge exited after unread hub output: %v", err)
	default:
	}

	writeAll(t, device, []byte("second-output-to-tcp\n"))
	readContains(t, tcpClient, []byte("second-output-to-tcp\n"), "tcp after unread hub")
	select {
	case err := <-errCh:
		t.Fatalf("ShareBridge exited after tcp delivery: %v", err)
	default:
	}

	closeStop()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ShareBridge stop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out stopping ShareBridge")
	}
}

func TestManualCom0ComPortsDocumented(t *testing.T) {
	if os.Getenv("SIO_COM0COM_INTEGRATION") != "1" {
		t.Skip("set SIO_COM0COM_INTEGRATION=1 to run against local com0com ports")
	}
	fmt.Fprintln(os.Stderr, "defaults: COM93<->CNCB93 as device pair, COM94<->CNCB94 as share pair, TCP 127.0.0.1:7003")
}
