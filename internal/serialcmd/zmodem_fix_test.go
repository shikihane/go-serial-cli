package serialcmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// zPeer is a scripted ZMODEM peer for tests: raw headers and subpackets on
// one end of a wire, with a log of every header the Go side sent.
type zPeer struct {
	t   *testing.T
	s   *zSession
	mu  sync.Mutex
	got []zHeader
}

func newZPeer(t *testing.T, end *zWireEnd) *zPeer {
	l := newZLink(end, nil)
	return &zPeer{t: t, s: &zSession{l: l, opts: &ZModemOptions{DataWait: 2 * time.Second}, res: &ZModemResult{}, out: os.Stderr, crc32: true}}
}

// next waits for a header from the Go side and logs it.
func (p *zPeer) next(wait time.Duration) (zHeader, error) {
	for {
		h, err := p.s.l.readHeader(wait)
		if errors.Is(err, errZBadHeader) {
			continue
		}
		if err == nil {
			p.mu.Lock()
			p.got = append(p.got, h)
			p.mu.Unlock()
		}
		return h, err
	}
}

func (p *zPeer) expect(t byte, wait time.Duration) zHeader {
	p.t.Helper()
	for {
		h, err := p.next(wait)
		if err != nil {
			p.t.Fatalf("waiting for %s: %v (got %v)", zFrameName(t), err, p.types())
		}
		if h.Type == t {
			return h
		}
	}
}

func (p *zPeer) types() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, h := range p.got {
		out = append(out, zFrameName(h.Type))
	}
	return out
}

func (p *zPeer) count(t byte) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, h := range p.got {
		if h.Type == t {
			n++
		}
	}
	return n
}

func (p *zPeer) offer(name string, data []byte, f0, f1 byte) {
	_ = p.s.sendBin(zFlagsHeader(zFILE, f0, f1, 0, 0))
	_ = p.s.sendData(zFileInfoBlock(name, int64(len(data)), time.Time{}, 1, int64(len(data))), zCRCW)
}

func (p *zPeer) sendFileData(data []byte, from int) {
	_ = p.s.sendBin(zPosHeader(zDATA, uint32(from)))
	for off := from; off < len(data); off += 1024 {
		end := min(off+1024, len(data))
		fe := zCRCG
		if end == len(data) {
			fe = zCRCE
		}
		_ = p.s.sendData(data[off:end], fe)
	}
	if from == len(data) {
		_ = p.s.sendData(nil, zCRCE)
	}
	_ = p.s.sendBin(zPosHeader(zEOF, uint32(len(data))))
}

func startGoReceiver(t *testing.T, end *zWireEnd, opts ZModemOptions) (chan error, *ZModemResult) {
	t.Helper()
	res := &ZModemResult{}
	opts.Link, opts.Receive, opts.Result = end, true, res
	if opts.HandshakeWait == 0 {
		opts.HandshakeWait = 10 * time.Second
	}
	if opts.DataWait == 0 {
		opts.DataWait = 2 * time.Second
	}
	opts.noDrain = true
	done := make(chan error, 1)
	go func() { done <- TransferZModem(opts) }()
	return done, res
}

func waitDone(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("go side did not finish")
		return nil
	}
}

// Item 1: a device repeats ZFILE every few seconds while the person keeps
// the Save dialog open. The queued repeats predate sio's answer and must
// not each get a ZRPOS.
func TestZModemRepeatedZFILEWhileChoosingGetsOneZRPOS(t *testing.T) {
	dst := t.TempDir()
	data := zPattern(50_000, 50)
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	dialog := make(chan struct{})
	done, res := startGoReceiver(t, a, ZModemOptions{Destination: func(info ZFileInfo) (ZDestination, error) {
		<-dialog
		return ZDestination{Path: filepath.Join(dst, info.Name)}, nil
	}})
	p := newZPeer(t, b)
	p.expect(zRINIT, 2*time.Second)
	for i := 0; i < 8; i++ {
		p.offer("r.bin", data, zCRESUM, zMCRC)
		time.Sleep(20 * time.Millisecond)
	}
	close(dialog)
	h := p.expect(zRPOS, 3*time.Second)
	// Repeats still in flight after the answer arrive late, too.
	p.offer("r.bin", data, zCRESUM, zMCRC)
	p.offer("r.bin", data, zCRESUM, zMCRC)
	time.Sleep(300 * time.Millisecond)
	p.sendFileData(data, int(h.pos()))
	p.expect(zRINIT, 3*time.Second)
	_ = p.s.l.sendHex(zPosHeader(zFIN, 0))
	p.expect(zFIN, 3*time.Second)
	_ = p.s.l.write([]byte("OO"))
	if err := waitDone(t, done); err != nil {
		t.Fatalf("receiver: %v (peer saw %v)", err, p.types())
	}
	mustEqualFile(t, filepath.Join(dst, "r.bin"), data)
	if n := p.count(zRPOS); n != 1 {
		t.Fatalf("ZRPOS sent %d times for one file: %v", n, p.types())
	}
	if len(res.Files) != 1 {
		t.Fatalf("files = %+v", res.Files)
	}
}

// Item 1, the lost-answer case: a repeat that comes long after our ZRPOS
// with no data yet means the ZRPOS was lost; it is answered once.
func TestZModemRepeatedZFILEAfterLostZRPOSIsAnsweredAgain(t *testing.T) {
	dst := t.TempDir()
	data := zPattern(10_000, 51)
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	done, _ := startGoReceiver(t, a, ZModemOptions{DestDir: dst})
	p := newZPeer(t, b)
	p.expect(zRINIT, 2*time.Second)
	p.offer("l.bin", data, zCBIN, 0)
	p.expect(zRPOS, 2*time.Second)
	time.Sleep(zRepeatGap + 200*time.Millisecond)
	p.offer("l.bin", data, zCBIN, 0)
	h := p.expect(zRPOS, 2*time.Second)
	p.sendFileData(data, int(h.pos()))
	p.expect(zRINIT, 3*time.Second)
	_ = p.s.l.sendHex(zPosHeader(zFIN, 0))
	p.expect(zFIN, 3*time.Second)
	if err := waitDone(t, done); err != nil {
		t.Fatalf("receiver: %v", err)
	}
	mustEqualFile(t, filepath.Join(dst, "l.bin"), data)
	if n := p.count(zRPOS); n != 2 {
		t.Fatalf("ZRPOS count = %d, want 2: %v", n, p.types())
	}
}

// Item 1, sender side: ZRINIT repeats queued while the open dialog was up
// are dropped, so only one ZFILE goes out.
func TestZModemSenderIgnoresZRINITQueuedDuringPicker(t *testing.T) {
	data := zPattern(5000, 52)
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	devDir := t.TempDir()
	recvDone := make(chan error, 1)
	go func() {
		recvDone <- TransferZModem(ZModemOptions{Link: b, Receive: true, DestDir: devDir, HandshakeWait: 30 * time.Second, noDrain: true})
	}()
	var zfiles int
	var mu sync.Mutex
	err := TransferZModem(ZModemOptions{Link: &zfileCounter{w: a, n: &zfiles, mu: &mu}, HandshakeWait: 10 * time.Second, noDrain: true,
		PickFiles: func() ([]ZSendFile, error) {
			// Long enough for the receiver to repeat its ZRINIT.
			time.Sleep(zRInitResend + 2*time.Second)
			return []ZSendFile{{Name: "p.bin", Data: data}}, nil
		}})
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	if err := waitDone(t, recvDone); err != nil {
		t.Fatalf("receiver: %v", err)
	}
	mustEqualFile(t, filepath.Join(devDir, "p.bin"), data)
	mu.Lock()
	defer mu.Unlock()
	if zfiles != 1 {
		t.Fatalf("ZFILE sent %d times", zfiles)
	}
}

// zfileCounter counts ZBIN32 ZFILE headers written through it.
type zfileCounter struct {
	w  *zWireEnd
	n  *int
	mu *sync.Mutex
}

func (c *zfileCounter) Read(b []byte) (int, error) { return c.w.Read(b) }
func (c *zfileCounter) Write(b []byte) (int, error) {
	c.mu.Lock()
	*c.n += bytes.Count(b, []byte{zPAD, zDLE, zBIN32, zFILE})
	c.mu.Unlock()
	return c.w.Write(b)
}

// Item 2: the receiver takes longer than zFileResend to answer ZFILE (it
// is computing a CRC); the sender keeps waiting and does not repeat its
// offer into a receiver that already spoke.
func TestZModemSenderWaitsForSlowZFILEAnswer(t *testing.T) {
	data := zPattern(20_000, 53)
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	var res ZModemResult
	done := make(chan error, 1)
	go func() {
		done <- TransferZModem(ZModemOptions{Link: a, Files: []ZSendFile{{Name: "s.bin", Data: data}}, HandshakeWait: 5 * time.Second, Result: &res, noDrain: true})
	}()
	p := newZPeer(t, b)
	p.expect(zRQINIT, 2*time.Second)
	_ = p.s.l.sendHex(zFlagsHeader(zRINIT, zCanFDX|zCanFC32, 0, 0, 0))
	p.expect(zFILE, 2*time.Second)
	_, _, _ = p.s.l.readSubpacket(make([]byte, 8192), 8192, true, time.Second)
	// Ask for a CRC, answer it, then stay silent longer than a resend.
	_ = p.s.l.sendHex(zPosHeader(zCRC, uint32(len(data))))
	p.expect(zCRC, 2*time.Second)
	silence := zFileResend + 2*time.Second
	h, err := p.next(silence)
	if err == nil {
		t.Fatalf("sender wrote %s while the receiver was busy", zFrameName(h.Type))
	}
	_ = p.s.l.sendHex(zPosHeader(zSKIP, 0))
	p.expect(zFIN, 3*time.Second)
	_ = p.s.l.sendHex(zPosHeader(zFIN, 0))
	if err := waitDone(t, done); err != nil {
		t.Fatalf("sender: %v", err)
	}
	if res.Files[0].Status != OutcomeSkipped {
		t.Fatalf("file = %+v", res.Files[0])
	}
}

func TestZModemAnswerWaitScalesWithSize(t *testing.T) {
	s := &zSession{opts: &ZModemOptions{HandshakeWait: 20 * time.Second}}
	if got := s.answerWait(0); got != defaultZHandshakeWait {
		t.Fatalf("answerWait(0) = %s, want the 120 s default floor", got)
	}
	if got := s.answerWait(50 << 20); got < defaultZHandshakeWait+10*time.Minute {
		t.Fatalf("answerWait(50MB) = %s, too short for a slow card", got)
	}
}

// Item 3: ZCRC only for ZCRESUM or ZMCRC offers.
func TestZModemReceiverSendsZCRCOnlyForResumeOrZMCRC(t *testing.T) {
	data := zPattern(30_000, 54)
	other := zPattern(30_000, 55)
	cases := []struct {
		name     string
		f0, f1   byte
		existing []byte
		part     []byte
		wantZCRC bool
		files    map[string][]byte
	}{
		{"plain-existing-renames-without-zcrc", zCBIN, 0, data, nil, false, map[string][]byte{"c.bin": data, "c_1.bin": data}},
		{"zmcrc-existing-identical-skips", zCBIN, zMCRC, data, nil, true, map[string][]byte{"c.bin": data}},
		{"zcresum-existing-identical-skips", zCRESUM, 0, data, nil, true, map[string][]byte{"c.bin": data}},
		{"zpart-without-zcresum-starts-over", zCBIN, 0, nil, other[:10_000], false, map[string][]byte{"c.bin": data}},
		{"zpart-with-zcresum-resumes", zCRESUM, 0, nil, data[:10_000], true, map[string][]byte{"c.bin": data}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			if tc.existing != nil {
				writeTemp(t, dst, "c.bin", tc.existing)
			}
			if tc.part != nil {
				writeTemp(t, dst, "c.bin.zpart", tc.part)
			}
			a, b := newZWire(zWireConfig{}, zWireConfig{})
			t.Cleanup(func() { _ = a.Close() })
			done, res := startGoReceiver(t, a, ZModemOptions{DestDir: dst})
			p := newZPeer(t, b)
			p.expect(zRINIT, 2*time.Second)
			p.offer("c.bin", data, tc.f0, tc.f1)
		answer:
			for {
				h, err := p.next(3 * time.Second)
				if err != nil {
					t.Fatalf("no answer to ZFILE: %v (%v)", err, p.types())
				}
				switch h.Type {
				case zCRC:
					n := int64(h.pos())
					if n == 0 {
						n = int64(len(data))
					}
					_ = p.s.sendBin(zPosHeader(zCRC, prefixCRC(bytes.NewReader(data), int64(len(data)), n)))
				case zRPOS:
					p.sendFileData(data, int(h.pos()))
					p.expect(zRINIT, 3*time.Second)
					break answer
				case zSKIP:
					break answer
				}
			}
			_ = p.s.l.sendHex(zPosHeader(zFIN, 0))
			p.expect(zFIN, 3*time.Second)
			if err := waitDone(t, done); err != nil {
				t.Fatalf("receiver: %v", err)
			}
			if got := p.count(zCRC) > 0; got != tc.wantZCRC {
				t.Fatalf("ZCRC sent = %v, want %v (%v)", got, tc.wantZCRC, p.types())
			}
			for name, want := range tc.files {
				mustEqualFile(t, filepath.Join(dst, name), want)
			}
			if tc.name == "zpart-with-zcresum-resumes" && res.Files[0].ResumeFrom != 10_000 {
				t.Fatalf("resume from %d", res.Files[0].ResumeFrom)
			}
		})
	}
}

// Item 5: ZABORT from the sender ends the receiver; it answers ZFIN and
// writes nothing after that.
func TestZModemReceiverHandlesZABORT(t *testing.T) {
	for _, when := range []string{"before-zfile", "during-data"} {
		t.Run(when, func(t *testing.T) {
			data := zPattern(20_000, 56)
			a, b := newZWire(zWireConfig{}, zWireConfig{})
			t.Cleanup(func() { _ = a.Close() })
			done, res := startGoReceiver(t, a, ZModemOptions{DestDir: t.TempDir()})
			p := newZPeer(t, b)
			p.expect(zRINIT, 2*time.Second)
			if when == "during-data" {
				p.offer("a.bin", data, zCBIN, 0)
				p.expect(zRPOS, 2*time.Second)
				_ = p.s.sendBin(zPosHeader(zDATA, 0))
				_ = p.s.sendData(data[:1024], zCRCE)
			}
			_ = p.s.l.sendHex(zPosHeader(zABORT, 0))
			p.expect(zFIN, 2*time.Second)
			err := waitDone(t, done)
			if !errors.Is(err, ErrTransferCancelled) || res.Outcome != OutcomeCancelled {
				t.Fatalf("err=%v outcome=%s", err, res.Outcome)
			}
			if h, err := p.next(500 * time.Millisecond); err == nil {
				t.Fatalf("receiver wrote %s after ZABORT", zFrameName(h.Type))
			}
			if bytes.Contains(p.s.l.pending, zCancelSequence[:5]) {
				t.Fatal("receiver sent a cancel sequence to a peer that aborted")
			}
		})
	}
}

func TestZModemSenderHandlesZABORT(t *testing.T) {
	data := zPattern(20_000, 57)
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	var res ZModemResult
	done := make(chan error, 1)
	go func() {
		done <- TransferZModem(ZModemOptions{Link: a, Files: []ZSendFile{{Name: "s.bin", Data: data}}, HandshakeWait: 5 * time.Second, Result: &res, noDrain: true})
	}()
	p := newZPeer(t, b)
	p.expect(zRQINIT, 2*time.Second)
	_ = p.s.l.sendHex(zFlagsHeader(zRINIT, zCanFDX|zCanFC32, 0, 0, 0))
	p.expect(zFILE, 2*time.Second)
	_ = p.s.l.sendHex(zPosHeader(zABORT, 0))
	err := waitDone(t, done)
	if !errors.Is(err, ErrTransferCancelled) || res.Outcome != OutcomeCancelled {
		t.Fatalf("err=%v outcome=%s", err, res.Outcome)
	}
}

// Item 5: after the device has given up (cancel sequence), sio does not
// keep writing ZRPOS or ZFILE into what is now the device shell.
func TestZModemWritesNothingAfterPeerCancel(t *testing.T) {
	for _, side := range []string{"receiver", "sender"} {
		t.Run(side, func(t *testing.T) {
			a, b := newZWire(zWireConfig{}, zWireConfig{})
			t.Cleanup(func() { _ = a.Close() })
			var done chan error
			if side == "receiver" {
				done, _ = startGoReceiver(t, a, ZModemOptions{DestDir: t.TempDir()})
			} else {
				done = make(chan error, 1)
				go func() {
					done <- TransferZModem(ZModemOptions{Link: a, Files: []ZSendFile{{Name: "x.bin", Data: []byte("x")}}, HandshakeWait: 5 * time.Second, noDrain: true})
				}()
			}
			p := newZPeer(t, b)
			if side == "receiver" {
				p.expect(zRINIT, 2*time.Second)
			} else {
				p.expect(zRQINIT, 2*time.Second)
			}
			// Drop the XON that trails the hex header just read.
			time.Sleep(50 * time.Millisecond)
			_ = p.s.l.poll()
			p.s.l.pending = nil
			_ = p.s.l.write(zCancelSequence)
			if err := waitDone(t, done); !errors.Is(err, ErrTransferCancelled) {
				t.Fatalf("err = %v", err)
			}
			deadline := time.Now().Add(500 * time.Millisecond)
			for time.Now().Before(deadline) {
				_ = p.s.l.fill(deadline)
			}
			if len(p.s.l.pending) > 0 {
				t.Fatalf("wrote %q after the peer cancelled", p.s.l.pending)
			}
		})
	}
}

// Item 5: damaged headers while a ZRPOS is out get no ZRPOS of their own.
func TestZModemBadHeadersDoNotStormZRPOS(t *testing.T) {
	dst := t.TempDir()
	data := zPattern(20_000, 58)
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	done, _ := startGoReceiver(t, a, ZModemOptions{DestDir: dst})
	p := newZPeer(t, b)
	p.expect(zRINIT, 2*time.Second)
	p.offer("b.bin", data, zCBIN, 0)
	p.expect(zRPOS, 2*time.Second)
	bad := encodeHexHeader(zPosHeader(zDATA, 0))
	bad[len(bad)-5] ^= 1
	for i := 0; i < 10; i++ {
		_ = p.s.l.write(bad)
		_ = p.s.sendBin(zPosHeader(zDATA, 999))
	}
	time.Sleep(300 * time.Millisecond)
	p.sendFileData(data, 0)
	p.expect(zRINIT, 3*time.Second)
	_ = p.s.l.sendHex(zPosHeader(zFIN, 0))
	p.expect(zFIN, 3*time.Second)
	if err := waitDone(t, done); err != nil {
		t.Fatalf("receiver: %v", err)
	}
	mustEqualFile(t, filepath.Join(dst, "b.bin"), data)
	if n := p.count(zRPOS); n != 1 {
		t.Fatalf("ZRPOS count = %d after 10 bad headers, want 1: %v", n, p.types())
	}
}

// Item 5: a ZRPOS at the sender's position (a repeated answer to ZFILE)
// neither halves the subpacket nor counts as a retransmission.
func TestZModemNonRewindingZRPOSDoesNotShrink(t *testing.T) {
	data := zPattern(60_000, 59)
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	var res ZModemResult
	done := make(chan error, 1)
	go func() {
		done <- TransferZModem(ZModemOptions{Link: a, Files: []ZSendFile{{Name: "n.bin", Data: data}}, HandshakeWait: 5 * time.Second, Result: &res, noDrain: true})
	}()
	p := newZPeer(t, b)
	p.expect(zRQINIT, 2*time.Second)
	_ = p.s.l.sendHex(zFlagsHeader(zRINIT, zCanFDX|zCanFC32, 0, 0, 0))
	p.expect(zFILE, 2*time.Second)
	for i := 0; i < 5; i++ {
		_ = p.s.l.sendHex(zPosHeader(zRPOS, 0))
	}
	// Receive the data like a real receiver.
	var got []byte
	buf := make([]byte, 8192)
	for {
		h := p.expect(zDATA, 3*time.Second)
		if int(h.pos()) != len(got) {
			t.Fatalf("ZDATA at %d, have %d", h.pos(), len(got))
		}
		for {
			d, end, err := p.s.l.readSubpacket(buf, 8192, true, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, d...)
			if end == zCRCQ || end == zCRCW {
				_ = p.s.l.sendHex(zPosHeader(zACK, uint32(len(got))))
			}
			if end == zCRCE || end == zCRCW {
				break
			}
		}
		if len(got) == len(data) {
			break
		}
	}
	p.expect(zEOF, 3*time.Second)
	_ = p.s.l.sendHex(zFlagsHeader(zRINIT, zCanFDX|zCanFC32, 0, 0, 0))
	p.expect(zFIN, 3*time.Second)
	_ = p.s.l.sendHex(zPosHeader(zFIN, 0))
	if err := waitDone(t, done); err != nil {
		t.Fatalf("sender: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("data differs")
	}
	f := res.Files[0]
	if f.Retransmits != 0 || f.MinBlock != defaultZSubpacket {
		t.Fatalf("retransmits=%d minblock=%d after ZRPOS that rewound nothing", f.Retransmits, f.MinBlock)
	}
}
