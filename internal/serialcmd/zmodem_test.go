package serialcmd

import (
	"bytes"
	"io"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func zPattern(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	data := make([]byte, n)
	_, _ = r.Read(data)
	return data
}

func zText(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = 'a' + byte(i%26)
	}
	return data
}

type zRun struct {
	sendErr, recvErr error
	send, recv       ZModemResult
	sendOut, recvOut bytes.Buffer
}

// runZPair runs a Go sender against a Go receiver over a and b.
func runZPair(t *testing.T, a, b io.ReadWriter, send, recv ZModemOptions) *zRun {
	t.Helper()
	r := &zRun{}
	send.Link, recv.Link = a, b
	recv.Receive = true
	send.Result, recv.Result = &r.send, &r.recv
	send.Output, recv.Output = &r.sendOut, &r.recvOut
	if send.HandshakeWait == 0 {
		send.HandshakeWait = 5 * time.Second
	}
	if recv.HandshakeWait == 0 {
		recv.HandshakeWait = 5 * time.Second
	}
	if send.DataWait == 0 {
		send.DataWait = 2 * time.Second
	}
	if recv.DataWait == 0 {
		recv.DataWait = 2 * time.Second
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.sendErr = TransferZModem(send) }()
	go func() { defer wg.Done(); r.recvErr = TransferZModem(recv) }()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("transfer hung\nsend:\n%s\nrecv:\n%s", r.sendOut.String(), r.recvOut.String())
	}
	return r
}

func zPipePair(t *testing.T) (net.Conn, net.Conn) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

func writeTemp(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustEqualFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: %d bytes, want %d; first diff at %d", path, len(got), len(want), firstDiff(got, want))
	}
}

func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// checkOutcome accepts the errors that report a refused file and wants
// both sides to agree on the file outcome.
func (r *zRun) checkOutcome(t *testing.T, file string) {
	t.Helper()
	if file != OutcomeRefused {
		r.check(t)
		return
	}
	if r.send.Outcome != OutcomeRefused || r.recv.Outcome != OutcomeRefused || r.sendErr == nil || r.recvErr == nil {
		t.Fatalf("refused file: send %s/%v recv %s/%v", r.send.Outcome, r.sendErr, r.recv.Outcome, r.recvErr)
	}
}

func (r *zRun) check(t *testing.T) {
	t.Helper()
	if r.sendErr != nil || r.recvErr != nil {
		t.Fatalf("send err=%v recv err=%v\nsend:\n%s\nrecv:\n%s", r.sendErr, r.recvErr, r.sendOut.String(), r.recvOut.String())
	}
}

func TestZModemGoToGoSendsFilesByteExact(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	files := map[string][]byte{
		"empty.bin": {},
		"small.txt": []byte("hello\r\n@\r\x18\x10\x11\x13\x91\x93\x7f\xff"),
		"big.bin":   zPattern(300_000, 1),
	}
	var send []ZSendFile
	for _, name := range []string{"empty.bin", "small.txt", "big.bin"} {
		send = append(send, ZSendFile{Path: writeTemp(t, src, name, files[name])})
	}
	a, b := zPipePair(t)
	r := runZPair(t, a, b, ZModemOptions{Files: send}, ZModemOptions{DestDir: dst})
	r.check(t)
	for name, data := range files {
		mustEqualFile(t, filepath.Join(dst, name), data)
	}
	if r.send.Outcome != OutcomeOK || r.recv.Outcome != OutcomeOK || len(r.recv.Files) != 3 {
		t.Fatalf("outcomes = %s/%s files=%d", r.send.Outcome, r.recv.Outcome, len(r.recv.Files))
	}
	if !r.send.CRC32 || !r.send.FullDuplex || r.send.Window != defaultZWindow {
		t.Fatalf("negotiation = %+v", r.send)
	}
	for _, name := range []string{"empty.bin", "small.txt", "big.bin"} {
		if _, err := os.Stat(filepath.Join(dst, name+".zpart")); err == nil {
			t.Fatalf("%s.zpart left behind", name)
		}
	}
}

func TestZModemWindowTakesSmallerOfReceiverBufferAndSendWindow(t *testing.T) {
	cases := []struct {
		name               string
		window, recvBuffer int
		want               int
	}{
		{"receiver-buffer-smaller", 16384, 4096, 4096},
		{"send-window-smaller", 4096, 16384, 4096},
		{"receiver-unlimited", 8192, 0, 8192},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			data := zPattern(100_000, 3)
			a, b := zPipePair(t)
			var maxInFlight int64
			send := ZModemOptions{Files: []ZSendFile{{Name: "w.bin", Data: data}}, Window: tc.window}
			send.trace = func(pos, acked int64) { maxInFlight = max(maxInFlight, pos-acked) }
			r := runZPair(t, a, b, send, ZModemOptions{DestDir: dst, RecvBuffer: tc.recvBuffer})
			r.check(t)
			mustEqualFile(t, filepath.Join(dst, "w.bin"), data)
			if r.send.Window != tc.want {
				t.Fatalf("window = %d, want %d", r.send.Window, tc.want)
			}
			// One subpacket may go out after the window check.
			if maxInFlight > int64(tc.want)+defaultZSubpacket {
				t.Fatalf("in flight reached %d with window %d", maxInFlight, tc.want)
			}
		})
	}
}

// With latency, a sender limited by its window blocks until a ZACK moves
// it, and keeps the link busy otherwise.
func TestZModemSenderBlocksAtWindowAndResumesOnAck(t *testing.T) {
	dst := t.TempDir()
	data := zPattern(256*1024, 4)
	const rate = 512 * 1024
	cfg := zWireConfig{latency: 20 * time.Millisecond, rate: rate}
	a, b := newZWire(cfg, cfg)
	t.Cleanup(func() { _ = a.Close() })
	const window = 8192
	var maxInFlight int64
	send := ZModemOptions{Files: []ZSendFile{{Name: "lat.bin", Data: data}}, Window: window}
	send.trace = func(pos, acked int64) { maxInFlight = max(maxInFlight, pos-acked) }
	r := runZPair(t, a, b, send, ZModemOptions{DestDir: dst})
	r.check(t)
	mustEqualFile(t, filepath.Join(dst, "lat.bin"), data)
	f := r.send.Files[0]
	if f.WindowBlocks == 0 {
		t.Fatal("sender never waited at the window with 20ms latency and an 8K window")
	}
	if maxInFlight > window+defaultZSubpacket {
		t.Fatalf("in flight reached %d with window %d", maxInFlight, window)
	}

	// A window that covers the bandwidth-delay product streams at the wire
	// rate: checkpoints do not stall it.
	dst2 := t.TempDir()
	a2, b2 := newZWire(cfg, cfg)
	t.Cleanup(func() { _ = a2.Close() })
	r2 := runZPair(t, a2, b2, ZModemOptions{Files: []ZSendFile{{Name: "lat.bin", Data: data}}, Window: 128 * 1024}, ZModemOptions{DestDir: dst2})
	r2.check(t)
	mustEqualFile(t, filepath.Join(dst2, "lat.bin"), data)
	wire := time.Duration(len(data)) * time.Second / rate
	if got := r2.send.Files[0].Elapsed; got > wire*3/2+200*time.Millisecond {
		t.Fatalf("large window took %s, wire time is %s", got, wire)
	}
	if got := r2.send.Files[0].WindowBlocks; got > 4 {
		t.Fatalf("large window blocked %d times", got)
	}
}

func TestZModemHalfDuplexReceiverGetsZCRCWBlocks(t *testing.T) {
	dst := t.TempDir()
	data := zPattern(20_000, 5)
	var crcw int
	cfg := zWireConfig{fault: func(_ int64, chunk []byte) []byte {
		crcw += bytes.Count(chunk, []byte{zDLE, zCRCW})
		return chunk
	}}
	a, b := newZWire(cfg, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	r := runZPair(t, a, b, ZModemOptions{Files: []ZSendFile{{Name: "hd.bin", Data: data}}}, ZModemOptions{DestDir: dst, halfDuplex: true})
	r.check(t)
	mustEqualFile(t, filepath.Join(dst, "hd.bin"), data)
	if r.send.FullDuplex {
		t.Fatal("sender did not see the half-duplex receiver")
	}
	// One ZCRCW per data block plus the file info block.
	if want := len(data)/defaultZSubpacket + 1; crcw < want {
		t.Fatalf("ZCRCW count = %d, want at least %d", crcw, want)
	}
}
