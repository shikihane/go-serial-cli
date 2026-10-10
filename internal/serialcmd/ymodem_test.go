package serialcmd

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestYModemCRC16KnownVector(t *testing.T) {
	// CRC-16/XMODEM of "123456789": poly 0x1021, init 0, no reflection, no xor.
	if got := crc16XModem([]byte("123456789")); got != 0x31C3 {
		t.Fatalf("crc = %04x, want 31c3", got)
	}
}

func TestYModemBlock0UsesSOHNameAndDecimalLength(t *testing.T) {
	frame, err := encodeYModemData(0, ymodemBlock0Body("note.bin", 1234), ymodemSOH)
	if err != nil {
		t.Fatal(err)
	}
	if frame[0] != ymodemSOH || frame[1] != 0x00 || frame[2] != 0xFF {
		t.Fatalf("header = %x, want SOH 00 FF", frame[:3])
	}
	body := frame[3 : 3+128]
	wantPrefix := []byte("note.bin\x001234\x00")
	if !bytes.HasPrefix(body, wantPrefix) {
		t.Fatalf("block 0 prefix = %q, want %q", body[:len(wantPrefix)], wantPrefix)
	}
	for _, b := range body[len(wantPrefix):] {
		if b != 0x00 {
			t.Fatal("block 0 padding is not 0x00")
		}
	}
	crc := crc16XModem(body)
	if frame[131] != byte(crc>>8) || frame[132] != byte(crc) {
		t.Fatalf("crc bytes = %02x %02x, want %04x high byte first", frame[131], frame[132], crc)
	}
}

func TestYModemBlock0AcceptsMtimeAfterSize(t *testing.T) {
	body := make([]byte, 128)
	copy(body, "note.bin\x0012 14752305671 100644\x00")
	name, size, err := parseYModemBlock0(body)
	if err != nil || name != "note.bin" || size != 12 {
		t.Fatalf("parse = %q %d %v", name, size, err)
	}
}

func TestYModemCleanLineDropsPromptErase(t *testing.T) {
	if got := cleanYModemLine("\x1b[2K\x1b[1;32m\rok note.bin 5\r"); got != "ok note.bin 5" {
		t.Fatalf("clean = %q", got)
	}
	// Captured from the device: the prompt is erased with cursor-left.
	line := cleanYModemLine("\x1b[1;32mdev$ \x1b[m\x1b[8D\x1b[Jexists")
	if line != "exists" || !isYModemResult(line) {
		t.Fatalf("clean = %q, want a result line", line)
	}
	if got := cleanYModemLine("ab\x1b[Dc"); got != "ac" {
		t.Fatalf("bare CSI D: clean = %q", got)
	}
}

// The fake device below follows the rz / sz firmware state machine.

func TestYModemSendToDeviceAfterReady(t *testing.T) {
	payload := patternPayload(3000)
	dev := startFakeDevice(t, func(d *fakeDevice) error {
		d.write("rz\r\n[CDC] Configured\r\nready\r\n")
		return d.receive(payload)
	})
	var out bytes.Buffer
	err := TransferYModem(YModemOptions{Address: dev.addr, SendFile: "note.bin", Payload: payload, Output: &out})
	if err != nil {
		t.Fatalf("TransferYModem: %v\n%s", err, out.String())
	}
	if err := dev.wait(); err != nil {
		t.Fatalf("device: %v", err)
	}
	if !strings.Contains(out.String(), "ok note.bin 3000") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestYModemSendToDeviceStartedEarlier(t *testing.T) {
	// The device command ran before sio sz. Only its C bytes are left, and
	// a log line containing C must not start the transfer.
	payload := patternPayload(1024)
	dev := startFakeDevice(t, func(d *fakeDevice) error {
		d.write("usb: CDC ready\r\n")
		return d.receive(payload)
	})
	err := TransferYModem(YModemOptions{Address: dev.addr, SendFile: "x.bin", Payload: payload})
	if err != nil {
		t.Fatalf("TransferYModem: %v", err)
	}
	if err := dev.wait(); err != nil {
		t.Fatalf("device: %v", err)
	}
}

func TestYModemSendRecoversFromRejectedPacket(t *testing.T) {
	payload := patternPayload(4096)
	dev := startFakeDevice(t, func(d *fakeDevice) error {
		d.rejectFirst = true
		d.write("ready\r\n")
		return d.receive(payload)
	})
	var out bytes.Buffer
	if err := TransferYModem(YModemOptions{Address: dev.addr, SendFile: "v.mp4", Payload: payload, Output: &out}); err != nil {
		t.Fatalf("TransferYModem: %v\n%s", err, out.String())
	}
	if err := dev.wait(); err != nil {
		t.Fatalf("device: %v", err)
	}
	if !strings.Contains(out.String(), "block 1: rejected with NAK; resending") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestYModemSendEmptyFile(t *testing.T) {
	dev := startFakeDevice(t, func(d *fakeDevice) error {
		d.write("ready\r\n")
		return d.receive([]byte{})
	})
	if err := TransferYModem(YModemOptions{Address: dev.addr, SendFile: "empty.bin", Payload: []byte{}}); err != nil {
		t.Fatalf("TransferYModem: %v", err)
	}
	if err := dev.wait(); err != nil {
		t.Fatalf("device: %v", err)
	}
}

func TestYModemReceiveFromDeviceStartedEarlier(t *testing.T) {
	// sz printed ready before sio rz connected. The device now waits
	// silently for C.
	payload := patternPayload(2500)
	dev := startFakeDevice(t, func(d *fakeDevice) error {
		return d.send("from.txt", payload)
	})
	var got bytes.Buffer
	status := YModemStatus{}
	err := TransferYModem(YModemOptions{Address: dev.addr, RecvWriter: &got, Status: &status})
	if err != nil {
		t.Fatalf("TransferYModem: %v", err)
	}
	if err := dev.wait(); err != nil {
		t.Fatalf("device: %v", err)
	}
	if !bytes.Equal(got.Bytes(), payload) || status.Name != "from.txt" {
		t.Fatalf("got %d bytes name %q", got.Len(), status.Name)
	}
}

func TestYModemReceiveFromDeviceAfterReady(t *testing.T) {
	payload := []byte("hello")
	dev := startFakeDevice(t, func(d *fakeDevice) error {
		time.Sleep(50 * time.Millisecond)
		d.write("sz from.txt\r\nready\r\n")
		return d.send("from.txt", payload)
	})
	var got bytes.Buffer
	if err := TransferYModem(YModemOptions{Address: dev.addr, RecvWriter: &got}); err != nil {
		t.Fatalf("TransferYModem: %v", err)
	}
	if err := dev.wait(); err != nil {
		t.Fatalf("device: %v", err)
	}
	if got.String() != "hello" {
		t.Fatalf("file = %q", got.String())
	}
}

func TestYModemReceiveRejectsDeviceWaitingForFile(t *testing.T) {
	dev := startFakeDevice(t, func(d *fakeDevice) error {
		for i := 0; i < 3; i++ {
			d.write("C")
			time.Sleep(time.Second)
		}
		return nil
	})
	var got bytes.Buffer
	err := TransferYModem(YModemOptions{Address: dev.addr, RecvWriter: &got, ReadyWait: 5 * time.Second})
	if !errors.Is(err, ErrDeviceWaitingForFile) {
		t.Fatalf("err = %v, want ErrDeviceWaitingForFile", err)
	}
}

func TestYModemResultExistsIsAnError(t *testing.T) {
	// rz rejects an existing name after block 0: CAN, then the reason.
	dev := startFakeDevice(t, func(d *fakeDevice) error {
		d.write("ready\r\nC")
		if code, ok := d.readByte(3 * time.Second); !ok || code != ymodemSOH {
			return errors.New("device wanted block 0")
		}
		if _, _, err := d.readFrame(ymodemSOH); err != nil {
			return err
		}
		d.write(strings.Repeat("\x18", 7))
		d.result("exists")
		return nil
	})
	err := TransferYModem(YModemOptions{Address: dev.addr, SendFile: "note.bin", Payload: []byte{1}, ReadyWait: time.Second})
	if err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("err = %v, want exists", err)
	}
}

func TestYModemSendStopsWhenDeviceGivesUp(t *testing.T) {
	// Seen on the board: the device RX ring overflowed, rz timed out on
	// block 0 and printed io. sio must report that, not retry blindly.
	dev := startFakeDevice(t, func(d *fakeDevice) error {
		d.write("ready\r\nC")
		if code, ok := d.readByte(3 * time.Second); !ok || code != ymodemSOH {
			return errors.New("device wanted block 0")
		}
		d.write("[shell_uart][W][126]: RX ring buffer full.\r\n")
		d.result("io")
		time.Sleep(time.Second)
		return nil
	})
	var out bytes.Buffer
	start := time.Now()
	err := TransferYModem(YModemOptions{Address: dev.addr, SendFile: "note.txt", Payload: []byte("hello"), Output: &out})
	if err == nil || !strings.Contains(err.Error(), "ymodem result: io") {
		t.Fatalf("err = %v, want ymodem result: io\n%s", err, out.String())
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %s; sio kept retrying after the device ended", time.Since(start))
	}
}

func TestYModemSendLogsProgress(t *testing.T) {
	payload := patternPayload(5000)
	dev := startFakeDevice(t, func(d *fakeDevice) error {
		d.write("ready\r\n")
		return d.receive(payload)
	})
	var out bytes.Buffer
	if err := TransferYModem(YModemOptions{Address: dev.addr, SendFile: "note.bin", Payload: payload, Output: &out}); err != nil {
		t.Fatalf("TransferYModem: %v\n%s", err, out.String())
	}
	for _, want := range []string{"claimed the session port", "sending block 0 name=\"note.bin\" size=5000", "sent 5000 bytes in", "ok note.bin 5000"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	t.Logf("\n%s", out.String())
}

func TestYModemProgressRates(t *testing.T) {
	var out bytes.Buffer
	l := &ymodemLink{output: &out}
	p := newYModemProgress(l, "sent", 400*1024)
	now := time.Now()
	p.start = now.Add(-4 * time.Second)
	p.last = now.Add(-2 * time.Second)
	p.lastDone = 100 * 1024
	p.update(300 * 1024)
	// 200 KB over the last 2 s, 300 KB over 4 s, 100 KB left at 75 KB/s.
	want := "ymodem: sent 307200/409600 bytes (75%) 100.0 KB/s, avg 75.0 KB/s, eta 1s\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
	out.Reset()
	p.update(310 * 1024)
	if out.Len() != 0 {
		t.Fatalf("printed again within a second: %q", out.String())
	}
}

func TestYModemReadyTimeout(t *testing.T) {
	dev := startFakeDevice(t, func(d *fakeDevice) error {
		d.write("still booting\r\n")
		time.Sleep(time.Second)
		return nil
	})
	err := TransferYModem(YModemOptions{Address: dev.addr, SendFile: "note.bin", Payload: []byte{1}, ReadyWait: 300 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "ready") {
		t.Fatalf("err = %v, want ready timeout", err)
	}
}

func patternPayload(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		// Includes 0x03, C, SOH and friends inside packet data.
		out[i] = byte(i * 7)
	}
	return out
}

type fakeDevice struct {
	addr string
	conn net.Conn
	done chan error
	// rejectFirst makes receive refuse the first data packet once, the way
	// rz does after losing bytes: purge, then NAK.
	rejectFirst bool
}

func startFakeDevice(t *testing.T, run func(d *fakeDevice) error) *fakeDevice {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDevice{addr: ln.Addr().String(), done: make(chan error, 1)}
	go func() {
		conn, err := ln.Accept()
		_ = ln.Close()
		if err != nil {
			d.done <- err
			return
		}
		d.conn = conn
		// Stand in for the session worker: take the claim first.
		claim, err := d.readN(len(ymodemClaimPreamble))
		if err != nil || string(claim) != ymodemClaimPreamble {
			d.done <- fmt.Errorf("claim = %q %v", claim, err)
			return
		}
		d.write(ymodemClaimOK)
		err = run(d)
		time.Sleep(100 * time.Millisecond)
		_ = conn.Close()
		d.done <- err
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return d
}

func (d *fakeDevice) wait() error {
	select {
	case err := <-d.done:
		return err
	case <-time.After(10 * time.Second):
		return errors.New("fake device did not finish")
	}
}

func (d *fakeDevice) write(s string) {
	_, _ = d.conn.Write([]byte(s))
}

func (d *fakeDevice) readByte(wait time.Duration) (byte, bool) {
	_ = d.conn.SetReadDeadline(time.Now().Add(wait))
	b := make([]byte, 1)
	n, _ := d.conn.Read(b)
	return b[0], n == 1
}

func (d *fakeDevice) readN(n int) ([]byte, error) {
	buf := make([]byte, n)
	got := 0
	_ = d.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for got < n {
		m, err := d.conn.Read(buf[got:])
		got += m
		if err != nil && got < n {
			return nil, err
		}
	}
	return buf, nil
}

func (d *fakeDevice) readFrame(header byte) (byte, []byte, error) {
	size := 128
	if header == ymodemSTX {
		size = 1024
	}
	rest, err := d.readN(2 + size + 2)
	if err != nil {
		return 0, nil, err
	}
	body := rest[2 : 2+size]
	if rest[0] != ^rest[1] || crc16XModem(body) != uint16(rest[2+size])<<8|uint16(rest[3+size]) {
		return 0, nil, errors.New("device saw a bad frame")
	}
	return rest[0], body, nil
}

func (d *fakeDevice) result(text string) {
	time.Sleep(100 * time.Millisecond)
	d.write("printk dma on\r\n\x1b[2K\r" + text + "\r\ndev$ ")
}

// receive mirrors rz: C every second, block 0, data, NAK/ACK on EOT,
// closing block 0.
func (d *fakeDevice) receive(want []byte) error {
	var code byte
	ok := false
	for i := 0; i < 30 && !ok; i++ {
		d.write("C")
		code, ok = d.readByte(time.Second)
		ok = ok && code == ymodemSOH
	}
	if !ok {
		return errors.New("device handshake timeout")
	}
	seq, body, err := d.readFrame(ymodemSOH)
	if err != nil || seq != 0 {
		return fmt.Errorf("block 0: seq %d %v", seq, err)
	}
	name, size, err := parseYModemBlock0(body)
	if err != nil || size != len(want) {
		return fmt.Errorf("block 0: %q %d %v", name, size, err)
	}
	d.write("\x06C")
	var got []byte
	expect := byte(1)
	for {
		code, ok := d.readByte(3 * time.Second)
		if !ok {
			return errors.New("device data timeout")
		}
		if code == ymodemEOT {
			break
		}
		if code != ymodemSOH && code != ymodemSTX {
			return fmt.Errorf("device got header %02x", code)
		}
		seq, body, err := d.readFrame(code)
		if err != nil {
			return err
		}
		if d.rejectFirst {
			d.rejectFirst = false
			time.Sleep(100 * time.Millisecond)
			d.write("\x15")
			continue
		}
		if seq != expect {
			return fmt.Errorf("device got block %d, want %d (duplicate data)", seq, expect)
		}
		got = append(got, body...)
		expect++
		d.write("\x06")
	}
	if len(got) > size {
		got = got[:size]
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("device stored %d bytes, want %d", len(got), len(want))
	}
	d.write("\x15")
	if code, ok := d.readByte(3 * time.Second); !ok || code != ymodemEOT {
		return errors.New("device wanted second EOT")
	}
	d.write("\x06C")
	if code, ok := d.readByte(3 * time.Second); !ok || code != ymodemSOH {
		return errors.New("device wanted closing block")
	}
	if _, _, err := d.readFrame(ymodemSOH); err != nil {
		return err
	}
	d.write("\x06")
	d.result(fmt.Sprintf("ok %s %d", name, size))
	return nil
}

// send mirrors sz: wait for C, block 0, ACK then C, STX blocks, EOT
// twice, wait for C, closing block 0 without waiting for its ACK.
func (d *fakeDevice) send(name string, data []byte) error {
	ok := false
	for i := 0; i < 30 && !ok; i++ {
		code, got := d.readByte(time.Second)
		ok = got && code == ymodemC
	}
	if !ok {
		return errors.New("device never got C")
	}
	expectSeq := func(want byte) error {
		code, ok := d.readByte(3 * time.Second)
		if !ok || code != want {
			return fmt.Errorf("device got %02x, want %02x", code, want)
		}
		return nil
	}
	block0, _ := encodeYModemData(0, ymodemBlock0Body(name, len(data)), ymodemSOH)
	_, _ = d.conn.Write(block0)
	if err := expectSeq(ymodemACK); err != nil {
		return err
	}
	if err := expectSeq(ymodemC); err != nil {
		return err
	}
	seq := byte(1)
	for off := 0; off < len(data); off += 1024 {
		body := bytes.Repeat([]byte{ymodemEOFPad}, 1024)
		copy(body, data[off:])
		frame, _ := encodeYModemData(seq, body, ymodemSTX)
		_, _ = d.conn.Write(frame)
		if err := expectSeq(ymodemACK); err != nil {
			return err
		}
		seq++
	}
	d.write("\x04")
	if err := expectSeq(ymodemNAK); err != nil {
		return err
	}
	d.write("\x04")
	if err := expectSeq(ymodemACK); err != nil {
		return err
	}
	if err := expectSeq(ymodemC); err != nil {
		return err
	}
	empty, _ := encodeYModemData(0, ymodemBlock0Body("", 0), ymodemSOH)
	_, _ = d.conn.Write(empty)
	d.result(fmt.Sprintf("ok %s %d", name, len(data)))
	return nil
}
