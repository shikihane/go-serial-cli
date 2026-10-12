package serialcmd

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// S1: the ZCRC request is asked again on silence and after a damaged
// answer, up to zCRCTries times; a good answer then resumes.
func TestZModemZCRCRequestRetried(t *testing.T) {
	for _, mode := range []string{"silence", "damaged-answer"} {
		t.Run(mode, func(t *testing.T) {
			dst := t.TempDir()
			data := zPattern(40_000, 60)
			writeTemp(t, dst, "c.bin.zpart", data[:10_000])
			a, b := newZWire(zWireConfig{}, zWireConfig{})
			t.Cleanup(func() { _ = a.Close() })
			done, res := startGoReceiver(t, a, ZModemOptions{DestDir: dst, crcWait: 3 * time.Second})
			p := newZPeer(t, b)
			p.expect(zRINIT, 2*time.Second)
			p.offer("c.bin", data, zCRESUM, 0)
			p.expect(zCRC, 2*time.Second)
			if mode == "damaged-answer" {
				bad := encodeBinHeader(zPosHeader(zCRC, prefixCRC(bytes.NewReader(data), int64(len(data)), 10_000)), true, &zEscaper{})
				bad[len(bad)-1] ^= 0x01
				_ = p.s.l.write(bad)
			}
			// Silence (or the damaged answer) brings a second request.
			h := p.expect(zCRC, 3*time.Second)
			_ = p.s.sendBin(zPosHeader(zCRC, prefixCRC(bytes.NewReader(data), int64(len(data)), int64(h.pos()))))
			rp := p.expect(zRPOS, 3*time.Second)
			if rp.pos() != 10_000 {
				t.Fatalf("ZRPOS %d, want resume at 10000", rp.pos())
			}
			p.sendFileData(data, int(rp.pos()))
			p.expect(zRINIT, 3*time.Second)
			_ = p.s.l.sendHex(zPosHeader(zFIN, 0))
			p.expect(zFIN, 3*time.Second)
			if err := waitDone(t, done); err != nil {
				t.Fatalf("receiver: %v", err)
			}
			mustEqualFile(t, filepath.Join(dst, "c.bin"), data)
			if res.Files[0].Status != OutcomeResumed {
				t.Fatalf("file = %+v", res.Files[0])
			}
		})
	}
}

// S1: with no answer at all, ZCRC goes out zCRCTries times and the file
// starts over at 0, never an unchecked resume.
func TestZModemZCRCGivesUpAfterTries(t *testing.T) {
	dst := t.TempDir()
	data := zPattern(20_000, 61)
	writeTemp(t, dst, "g.bin.zpart", data[:5_000])
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	done, _ := startGoReceiver(t, a, ZModemOptions{DestDir: dst, crcWait: 1500 * time.Millisecond})
	p := newZPeer(t, b)
	p.expect(zRINIT, 2*time.Second)
	p.offer("g.bin", data, zCRESUM, 0)
	rp := p.expect(zRPOS, 5*time.Second)
	if rp.pos() != 0 {
		t.Fatalf("ZRPOS %d without a CRC answer, want 0", rp.pos())
	}
	if n := p.count(zCRC); n != zCRCTries {
		t.Fatalf("ZCRC asked %d times, want %d", n, zCRCTries)
	}
	p.sendFileData(data, 0)
	p.expect(zRINIT, 3*time.Second)
	_ = p.s.l.sendHex(zPosHeader(zFIN, 0))
	p.expect(zFIN, 3*time.Second)
	if err := waitDone(t, done); err != nil {
		t.Fatalf("receiver: %v", err)
	}
	mustEqualFile(t, filepath.Join(dst, "g.bin"), data)
}

// S2: a plain ZSKIP (the device refusing an existing name) is "refused",
// distinct from an identical skip after a ZCRC exchange; a batch with a
// refusal is partial, not ok.
func TestZModemSenderReportsRefusalAndPartialBatch(t *testing.T) {
	data := zPattern(8000, 62)
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	var res ZModemResult
	done := make(chan error, 1)
	go func() {
		done <- TransferZModem(ZModemOptions{Link: a, HandshakeWait: 5 * time.Second, Result: &res, noDrain: true, Files: []ZSendFile{
			{Name: "new.bin", Data: data}, {Name: "taken.bin", Data: data}, {Name: "same.bin", Data: data},
		}})
	}()
	p := newZPeer(t, b)
	p.expect(zRQINIT, 2*time.Second)
	rinit := zFlagsHeader(zRINIT, zCanFDX|zCanFC32, 0, 0, 0)
	_ = p.s.l.sendHex(rinit)
	buf := make([]byte, 8192)
	// new.bin: take it.
	p.expect(zFILE, 2*time.Second)
	_, _, _ = p.s.l.readSubpacket(buf, 8192, true, time.Second)
	_ = p.s.l.sendHex(zPosHeader(zRPOS, 0))
	for {
		h := p.expect(zDATA, 3*time.Second)
		_ = h
		var end byte
		for end != zCRCE {
			_, e, err := p.s.l.readSubpacket(buf, 8192, true, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			end = e
		}
		break
	}
	p.expect(zEOF, 2*time.Second)
	_ = p.s.l.sendHex(rinit)
	// taken.bin: refuse without comparing.
	p.expect(zFILE, 2*time.Second)
	_, _, _ = p.s.l.readSubpacket(buf, 8192, true, time.Second)
	_ = p.s.l.sendHex(zPosHeader(zSKIP, 0))
	// same.bin: compare, then skip.
	p.expect(zFILE, 2*time.Second)
	_, _, _ = p.s.l.readSubpacket(buf, 8192, true, time.Second)
	_ = p.s.l.sendHex(zPosHeader(zCRC, 0))
	p.expect(zCRC, 2*time.Second)
	_ = p.s.l.sendHex(zPosHeader(zSKIP, 0))
	p.expect(zFIN, 3*time.Second)
	_ = p.s.l.sendHex(zPosHeader(zFIN, 0))
	err := waitDone(t, done)
	if err == nil || res.Outcome != OutcomePartial {
		t.Fatalf("err=%v outcome=%s, want partial", err, res.Outcome)
	}
	want := []string{OutcomeOK, OutcomeRefused, OutcomeSkipped}
	for i, f := range res.Files {
		if f.Status != want[i] {
			t.Fatalf("file %d %s: %s (%s), want %s", i, f.Name, f.Status, f.Reason, want[i])
		}
	}
}

func TestSessionOutcome(t *testing.T) {
	f := func(st ...string) []ZFileResult {
		var out []ZFileResult
		for _, s := range st {
			out = append(out, ZFileResult{Status: s})
		}
		return out
	}
	for _, tc := range []struct {
		files []ZFileResult
		want  string
	}{
		{f(OutcomeOK, OutcomeOK), OutcomeOK},
		{f(OutcomeOK, OutcomeSkipped), OutcomeOK},
		{f(OutcomeSkipped), OutcomeSkipped},
		{f(OutcomeOK, OutcomeResumed), OutcomeResumed},
		{f(OutcomeRefused), OutcomeRefused},
		{f(OutcomeRefused, OutcomeFailed), OutcomeFailed},
		{f(OutcomeOK, OutcomeRefused), OutcomePartial},
		{f(OutcomeSkipped, OutcomeFailed), OutcomePartial},
		{f(OutcomeFailed), OutcomeFailed},
	} {
		if got := sessionOutcome(tc.files); got != tc.want {
			t.Errorf("%v -> %s, want %s", tc.files, got, tc.want)
		}
	}
}

// S3: a ZRPOS at the sender's current offset while a frame is open (the
// receiver lost the header and is hunting) closes the frame with an empty
// ZCRCE and starts a fresh ZDATA there, without rewinding anything.
func TestZModemZRPOSAtCurrentOffsetRestartsFrame(t *testing.T) {
	data := zPattern(60_000, 63)
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	var res ZModemResult
	done := make(chan error, 1)
	go func() {
		done <- TransferZModem(ZModemOptions{Link: a, HandshakeWait: 5 * time.Second, Result: &res, noDrain: true, Window: 8192,
			Files: []ZSendFile{{Name: "f.bin", Data: data}}})
	}()
	p := newZPeer(t, b)
	p.expect(zRQINIT, 2*time.Second)
	_ = p.s.l.sendHex(zFlagsHeader(zRINIT, zCanFDX|zCanFC32, 0, 0, 0))
	buf := make([]byte, 8192)
	p.expect(zFILE, 2*time.Second)
	_, _, _ = p.s.l.readSubpacket(buf, 8192, true, time.Second)
	_ = p.s.l.sendHex(zPosHeader(zRPOS, 0))
	p.expect(zDATA, 2*time.Second)
	// Take subpackets without ACKing until the sender stalls at its window.
	var got []byte
	for {
		d, _, err := p.s.l.readSubpacket(buf, 8192, true, time.Second)
		if errors.Is(err, errZTimeout) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, d...)
	}
	if len(got) != 8192 {
		t.Fatalf("sender stalled at %d, want the 8192 window", len(got))
	}
	_ = p.s.l.sendHex(zPosHeader(zRPOS, uint32(len(got))))
	// The open frame ends with an empty ZCRCE, then a fresh header.
	d, end, err := p.s.l.readSubpacket(buf, 8192, true, 2*time.Second)
	if err != nil || len(d) != 0 || end != zCRCE {
		t.Fatalf("after ZRPOS: %d bytes end=%q err=%v, want an empty ZCRCE", len(d), end, err)
	}
	for len(got) < len(data) {
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
	if f := res.Files[0]; f.Retransmits != 0 || f.MinBlock != defaultZSubpacket {
		t.Fatalf("retransmits=%d min=%d for a ZRPOS that rewound nothing", f.Retransmits, f.MinBlock)
	}
}

// S4: once the sender has spoken in a phase, ZRINIT is no longer repeated
// on a timer; a ZRQINIT still gets one at once.
func TestZModemReceiverStopsTimedZRINITAfterPeerSpoke(t *testing.T) {
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	done, _ := startGoReceiver(t, a, ZModemOptions{DestDir: t.TempDir(), HandshakeWait: 30 * time.Second})
	p := newZPeer(t, b)
	p.expect(zRINIT, 2*time.Second)
	_ = p.s.l.sendHex(zPosHeader(zCHALLENGE, 7))
	p.expect(zACK, 2*time.Second)
	if h, err := p.next(zRInitResend + 2*time.Second); err == nil {
		t.Fatalf("receiver sent %s after the peer spoke", zFrameName(h.Type))
	}
	_ = p.s.l.sendHex(zPosHeader(zRQINIT, 0))
	p.expect(zRINIT, 2*time.Second)
	_ = p.s.l.sendHex(zPosHeader(zFIN, 0))
	p.expect(zFIN, 2*time.Second)
	if err := waitDone(t, done); err != nil {
		t.Fatalf("receiver: %v", err)
	}
}

// S4: before the peer speaks, ZRINIT is repeated on silence.
func TestZModemReceiverRepeatsZRINITWhilePeerSilent(t *testing.T) {
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	done, _ := startGoReceiver(t, a, ZModemOptions{DestDir: t.TempDir(), HandshakeWait: 30 * time.Second})
	p := newZPeer(t, b)
	p.expect(zRINIT, 2*time.Second)
	p.expect(zRINIT, zRInitResend+2*time.Second)
	_ = p.s.l.sendHex(zPosHeader(zFIN, 0))
	p.expect(zFIN, 2*time.Second)
	if err := waitDone(t, done); err != nil {
		t.Fatalf("receiver: %v", err)
	}
}

// failingReader fails reads at or after off.
type failingReader struct {
	data []byte
	off  int64
}

func (r *failingReader) ReadAt(b []byte, at int64) (int, error) {
	if at >= r.off {
		return 0, errors.New("disk read error")
	}
	n := copy(b, r.data[at:min(int64(len(r.data)), r.off)])
	return n, nil
}

// A read error on one file cuts it short and the batch goes on.
func TestZModemSenderReadErrorSkipsFile(t *testing.T) {
	dst := t.TempDir()
	good := zPattern(20_000, 64)
	a, b := zPipePair(t)
	send := ZModemOptions{Files: []ZSendFile{{Name: "bad.bin", Data: zPattern(50_000, 65)}, {Name: "good.bin", Data: good}}}
	send.sourceHook = func(name string, src *zSource) {
		if name == "bad.bin" {
			src.r = &failingReader{data: zPattern(50_000, 65), off: 12_000}
		}
	}
	r := runZPair(t, a, b, send, ZModemOptions{DestDir: dst})
	mustEqualFile(t, filepath.Join(dst, "good.bin"), good)
	if r.send.Outcome != OutcomePartial || len(r.send.Files) != 2 || r.send.Files[0].Status != OutcomeFailed || r.send.Files[1].Status != OutcomeOK {
		t.Fatalf("send outcome=%s files=%+v err=%v", r.send.Outcome, r.send.Files, r.sendErr)
	}
}

// The link reader goroutine ends with the session even when nobody reads
// what it holds.
func TestZLinkReaderStopsOnClose(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		pr, pw := io.Pipe()
		go func() {
			for {
				if _, err := pw.Write(make([]byte, 1024)); err != nil {
					return
				}
			}
		}()
		l := newZLink(struct {
			io.Reader
			io.Writer
		}{pr, io.Discard}, nil)
		time.Sleep(20 * time.Millisecond)
		l.close()
		_ = pr.Close()
	}
	time.Sleep(200 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+3 {
		t.Fatalf("goroutines %d -> %d: link readers leaked", before, after)
	}
}

// Half-duplex: a receiver that keeps answering with stray headers that do
// not move the window cannot hold the sender forever.
func TestZModemHalfDuplexWaitAckIsBounded(t *testing.T) {
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	done := make(chan error, 1)
	go func() {
		done <- TransferZModem(ZModemOptions{Link: a, HandshakeWait: 5 * time.Second, noDrain: true, Files: []ZSendFile{{Name: "h.bin", Data: zPattern(5000, 66)}}})
	}()
	p := newZPeer(t, b)
	p.expect(zRQINIT, 2*time.Second)
	_ = p.s.l.sendHex(zFlagsHeader(zRINIT, zCanFC32, 0, 0, 0))
	buf := make([]byte, 8192)
	p.expect(zFILE, 2*time.Second)
	_, _, _ = p.s.l.readSubpacket(buf, 8192, true, time.Second)
	_ = p.s.l.sendHex(zPosHeader(zRPOS, 0))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(500 * time.Millisecond):
				_ = p.s.l.sendHex(zPosHeader(zNAK, 0))
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("sender finished without any ZACK")
		}
	case <-time.After(zAckPokeWait*(zAckPokes+2) + 10*time.Second):
		t.Fatal("half-duplex sender waited forever")
	}
}

// The device gives up while a person still has the dialog open: the
// dialog is closed and nothing more is written to the device.
func TestZModemPeerCancelDuringDialogClosesPicker(t *testing.T) {
	for _, side := range []string{"save-dialog", "open-dialog"} {
		t.Run(side, func(t *testing.T) {
			a, b := newZWire(zWireConfig{}, zWireConfig{})
			t.Cleanup(func() { _ = a.Close() })
			closed := make(chan struct{})
			var once sync.Once
			closePicker := func() { once.Do(func() { close(closed) }) }
			dialog := func() error {
				select {
				case <-closed:
					return ErrTransferCancelled
				case <-time.After(20 * time.Second):
					return errors.New("dialog never closed")
				}
			}
			opts := ZModemOptions{Link: a, HandshakeWait: 30 * time.Second, noDrain: true, ClosePicker: closePicker}
			if side == "save-dialog" {
				opts.Receive = true
				opts.Destination = func(ZFileInfo) (ZDestination, error) { return ZDestination{}, dialog() }
			} else {
				opts.PickFiles = func() ([]ZSendFile, error) { return nil, dialog() }
			}
			var res ZModemResult
			opts.Result = &res
			done := make(chan error, 1)
			go func() { done <- TransferZModem(opts) }()
			p := newZPeer(t, b)
			if side == "save-dialog" {
				p.expect(zRINIT, 2*time.Second)
				p.offer("d.bin", []byte("data"), zCRESUM, zMCRC)
			} else {
				p.expect(zRQINIT, 2*time.Second)
				_ = p.s.l.sendHex(zFlagsHeader(zRINIT, zCanFDX|zCanFC32, 0, 0, 0))
			}
			time.Sleep(300 * time.Millisecond)
			_ = p.s.l.poll()
			p.s.l.pending = nil
			start := time.Now()
			_ = p.s.l.write(zCancelSequence)
			err := waitDone(t, done)
			if !errors.Is(err, ErrTransferCancelled) || res.Outcome != OutcomeCancelled {
				t.Fatalf("err=%v outcome=%s", err, res.Outcome)
			}
			if time.Since(start) > 3*time.Second {
				t.Fatalf("took %s to notice the device gave up", time.Since(start))
			}
			select {
			case <-closed:
			default:
				t.Fatal("picker was not closed")
			}
			deadline := time.Now().Add(500 * time.Millisecond)
			for time.Now().Before(deadline) {
				_ = p.s.l.fill(deadline)
			}
			if len(p.s.l.pending) > 0 {
				t.Fatalf("wrote %q after the device gave up", p.s.l.pending)
			}
		})
	}
}

// Ctrl+C in the shell: every press goes to the transfer; the second one
// also forces the wait for the device result line to end.
func TestShellGateCtrlCPresses(t *testing.T) {
	g := newShellGate()
	cancel := g.begin()
	force := g.forced()
	g.feed([]byte{0x03})
	select {
	case <-cancel:
	default:
		t.Fatal("first Ctrl+C did not cancel")
	}
	select {
	case <-force:
		t.Fatal("first Ctrl+C forced")
	default:
	}
	g.feed([]byte{0x03})
	g.feed([]byte{0x03})
	select {
	case <-force:
	default:
		t.Fatal("second Ctrl+C did not force")
	}
	if !g.feed([]byte("x")) {
		t.Fatal("keys leaked to the device during the transfer")
	}
}

// A forced cancel does not wait the full result window for device text.
func TestZModemForceSkipsResultWait(t *testing.T) {
	a, b := newZWire(zWireConfig{}, zWireConfig{})
	t.Cleanup(func() { _ = a.Close() })
	cancel, force := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- TransferZModem(ZModemOptions{Link: a, Receive: true, DestDir: t.TempDir(), HandshakeWait: 30 * time.Second, Cancel: cancel, Force: force})
	}()
	p := newZPeer(t, b)
	p.expect(zRINIT, 2*time.Second)
	// Device text keeps the drain busy.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				_ = p.s.l.write([]byte("log line\r\n"))
			}
		}
	}()
	defer close(stop)
	close(cancel)
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	close(force)
	select {
	case err := <-done:
		if !errors.Is(err, ErrTransferCancelled) {
			t.Fatalf("err = %v", err)
		}
		if time.Since(start) > time.Second {
			t.Fatalf("force took %s", time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forced cancel still waiting")
	}
}
