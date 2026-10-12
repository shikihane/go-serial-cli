package serialcmd

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"sync"
	"time"
)

var (
	errZTimeout      = errors.New("timed out")
	errZBadHeader    = errors.New("bad header")
	errZBadCRC       = errors.New("bad subpacket crc")
	errZOverflow     = errors.New("subpacket too long")
	errZPeerCancel   = errors.New("cancelled by peer")
	errZPeerAbort    = errors.New("aborted by peer")
	errZLocalCancel  = errors.New("cancelled")
	errZLinkClosed   = errors.New("link closed")
	errZUnexpectedFE = errors.New("unexpected frame end")
)

// zChunk is one read from the link, handed over by the reader goroutine.
type zChunk struct {
	data []byte
	err  error
	// at is when the read returned.
	at time.Time
}

// zLink is the byte link under one ZMODEM session. A goroutine reads the
// link so that every wait can time out and the sender can look at the
// reverse channel between subpackets without blocking.
type zLink struct {
	w       io.Writer
	rx      chan zChunk
	rxErr   error
	pending []byte
	cancel  <-chan struct{}

	// ctlIgnore drops raw control bytes: the peer was asked to escape them.
	ctlIgnore bool

	// echo, when set, receives device text lines seen outside frames.
	echo io.Writer
	text []byte

	// gone is set once the peer has given up (cancel sequence, ZABORT,
	// ZFERR). Nothing more is written: the bytes would land in the
	// device shell as a command line.
	gone bool
	// held is a header read ahead and put back.
	held *zHeader
	// rxAt is when the newest bytes in pending arrived; no byte in
	// pending arrived later.
	rxAt time.Time
	// flow makes raw XON/XOFF from the peer flow control, for a sender:
	// xoff holds its data until XON. A receiver escapes them inside
	// headers, so they are taken out of pending.
	flow bool
	xoff bool
	// done stops the reader goroutine once the session is over, so it
	// does not block forever on a full channel.
	done      chan struct{}
	closeOnce sync.Once

	bytesIn  int64
	bytesOut int64
}

func newZLink(rw io.ReadWriter, cancel <-chan struct{}) *zLink {
	l := &zLink{w: rw, rx: make(chan zChunk, 64), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(l.rx)
		for {
			buf := make([]byte, 16*1024)
			n, err := rw.Read(buf)
			if n > 0 {
				select {
				case l.rx <- zChunk{data: buf[:n], at: time.Now()}:
				case <-l.done:
					return
				}
			}
			if err != nil {
				select {
				case l.rx <- zChunk{err: err}:
				case <-l.done:
				}
				return
			}
		}
	}()
	return l
}

// close ends the session's use of the link. The reader goroutine exits at
// its next read instead of waiting for someone to take the chunk.
func (l *zLink) close() {
	l.closeOnce.Do(func() { close(l.done) })
}

// peerCancelled reports whether the bytes waiting already hold the
// cancel sequence (five CAN in a row), without consuming anything.
func (l *zLink) peerCancelled() bool {
	if l.gone {
		return true
	}
	if bytes.Contains(l.pending, zCancelSequence[:5]) {
		l.gone = true
	}
	return l.gone
}

// watchCancel reads ahead until stop is closed and reports a peer cancel
// sequence as soon as it shows up. The bytes read stay in pending.
func (l *zLink) watchCancel(stop <-chan struct{}, gone func()) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		if l.peerCancelled() {
			gone()
			return
		}
		if err := l.fill(time.Now().Add(50 * time.Millisecond)); err != nil && !errors.Is(err, errZTimeout) {
			return
		}
	}
}

func (l *zLink) write(data []byte) error {
	if l.gone {
		return errZPeerCancel
	}
	for len(data) > 0 {
		n, err := l.w.Write(data)
		l.bytesOut += int64(n)
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func (l *zLink) take(c zChunk, ok bool) {
	if !ok {
		if l.rxErr == nil {
			l.rxErr = errZLinkClosed
		}
		return
	}
	if c.err != nil {
		if errors.Is(c.err, io.EOF) {
			l.rxErr = errZLinkClosed
		} else {
			l.rxErr = c.err
		}
		return
	}
	l.bytesIn += int64(len(c.data))
	l.rxAt = c.at
	data := c.data
	if l.flow {
		data = l.flowControl(data)
	}
	l.pending = append(l.pending, data...)
}

// flowControl takes raw XON/XOFF out of data; the last one decides
// whether the sender is held.
func (l *zLink) flowControl(data []byte) []byte {
	out := data[:0]
	for _, b := range data {
		switch b & 0x7f {
		case asciiXOFF:
			l.xoff = true
		case asciiXON:
			l.xoff = false
		default:
			out = append(out, b)
		}
	}
	return out
}

// fill waits until more bytes arrive or the deadline passes.
func (l *zLink) fill(deadline time.Time) error {
	if l.rxErr != nil {
		return l.rxErr
	}
	select {
	case <-l.cancel:
		return errZLocalCancel
	default:
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		select {
		case c, ok := <-l.rx:
			l.take(c, ok)
			return nil
		default:
			return errZTimeout
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case c, ok := <-l.rx:
		l.take(c, ok)
		return nil
	case <-timer.C:
		return errZTimeout
	case <-l.cancel:
		return errZLocalCancel
	}
}

// poll moves every chunk that has already arrived into pending.
func (l *zLink) poll() error {
	for {
		select {
		case c, ok := <-l.rx:
			l.take(c, ok)
			if !ok || c.err != nil {
				return l.rxErr
			}
		case <-l.cancel:
			return errZLocalCancel
		default:
			return nil
		}
	}
}

func (l *zLink) readByte(deadline time.Time) (byte, error) {
	for len(l.pending) == 0 {
		if l.rxErr != nil {
			return 0, l.rxErr
		}
		if err := l.fill(deadline); err != nil {
			return 0, err
		}
	}
	b := l.pending[0]
	l.pending = l.pending[1:]
	return b, nil
}

// drop discards n pending bytes that are not part of a frame. With echo
// on they are collected as device text.
func (l *zLink) drop(n int) {
	if l.echo != nil {
		l.noteText(l.pending[:n])
	}
	l.pending = l.pending[n:]
}

func (l *zLink) noteText(data []byte) {
	for _, b := range data {
		switch {
		case b == '\r' || b == '\n':
			l.flushText()
		case len(l.text) < 512:
			l.text = append(l.text, b)
		}
	}
}

func (l *zLink) flushText() {
	line := cleanDeviceLine(string(l.text))
	l.text = l.text[:0]
	if l.echo != nil && isDeviceTextLine(line) {
		_, _ = fmt.Fprintln(l.echo, line)
	}
}

// isDeviceTextLine keeps lines a person would want to see and drops the
// protocol leftovers that can surround a session: "rz", "OO" and bytes
// that are not text.
func isDeviceTextLine(line string) bool {
	if line == "" || line == "rz" || strings.Trim(line, "O") == "" {
		return false
	}
	letters := 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c < 0x20 || c >= 0x7f {
			return false
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			letters++
		}
	}
	return letters > 0
}

const (
	zScanNeedMore = iota
	zScanFound
	zScanBad
	zScanCancel
)

// zScanHeader looks for one header in buf. Bytes before start are not part
// of any frame. For zScanFound and zScanBad, end is where scanning goes
// on; for zScanNeedMore, buf[start:] may still become a header.
func zScanHeader(buf []byte, ctlIgnore bool) (h zHeader, start int, end int, status int) {
	i := 0
	for i < len(buf) {
		c := buf[i]
		if c == asciiCAN {
			j := i
			for j < len(buf) && buf[j] == asciiCAN {
				j++
			}
			if j-i >= 5 {
				return h, i, j, zScanCancel
			}
			if j == len(buf) {
				return h, i, i, zScanNeedMore
			}
			i = j
			continue
		}
		if c&0x7f != zPAD {
			i++
			continue
		}
		j := i
		for j < len(buf) && buf[j]&0x7f == zPAD {
			j++
		}
		if j == len(buf) {
			return h, i, i, zScanNeedMore
		}
		if buf[j] != zDLE {
			i = j
			continue
		}
		j++
		if j == len(buf) {
			return h, i, i, zScanNeedMore
		}
		format := buf[j] & 0x7f
		j++
		switch format {
		case zHEX:
			if len(buf)-j < 14 {
				return h, i, i, zScanNeedMore
			}
			hdr, ok := decodeHexHeaderDigits(buf[j : j+14])
			if !ok {
				return h, i, i + 1, zScanBad
			}
			j += 14
			// Throw away the CR LF that follows a hex header.
			if j < len(buf) && buf[j]&0x7f == '\r' {
				j++
				if j < len(buf) && buf[j]&0x7f == '\n' {
					j++
				}
			}
			return hdr, i, j, zScanFound
		case zBIN, zBIN32:
			want := 7
			if format == zBIN32 {
				want = 9
			}
			vals, used, st := zUnescapeN(buf[j:], want, ctlIgnore)
			switch st {
			case zScanNeedMore:
				return h, i, i, zScanNeedMore
			case zScanCancel:
				return h, i, j + used, zScanCancel
			case zScanBad:
				return h, i, i + 1, zScanBad
			}
			hdr := zHeader{Type: vals[0], Data: [4]byte{vals[1], vals[2], vals[3], vals[4]}, Format: format}
			if format == zBIN32 {
				if crc32.ChecksumIEEE(vals[:5]) != binary.LittleEndian.Uint32(vals[5:9]) {
					return h, i, i + 1, zScanBad
				}
			} else if crc16XModem(vals[:5]) != uint16(vals[5])<<8|uint16(vals[6]) {
				return h, i, i + 1, zScanBad
			}
			return hdr, i, j + used, zScanFound
		default:
			i++
		}
	}
	return h, len(buf), len(buf), zScanNeedMore
}

// zUnescapeN decodes n ZDLE-escaped values from buf, as lrzsz zdlread does.
// A frame end inside a header is an error.
func zUnescapeN(buf []byte, n int, ctlIgnore bool) ([]byte, int, int) {
	vals := make([]byte, 0, n)
	i := 0
	for len(vals) < n {
		if i >= len(buf) {
			return nil, i, zScanNeedMore
		}
		c := buf[i]
		i++
		if c&0x60 != 0 {
			vals = append(vals, c)
			continue
		}
		switch {
		case c == zDLE:
		case isXonXoff(c), ctlIgnore:
			continue
		default:
			vals = append(vals, c)
			continue
		}
		cans := 1
		for {
			if i >= len(buf) {
				return nil, i, zScanNeedMore
			}
			c = buf[i]
			i++
			if c == asciiCAN {
				cans++
				if cans >= 5 {
					return nil, i, zScanCancel
				}
				continue
			}
			if isXonXoff(c) || (ctlIgnore && c&0x60 == 0) {
				continue
			}
			break
		}
		if cans > 1 {
			// ZDLE followed by fewer than four CAN: not valid escaping.
			return nil, i, zScanBad
		}
		switch {
		case c == zRUB0:
			vals = append(vals, 0x7f)
		case c == zRUB1:
			vals = append(vals, 0xff)
		case c&0x60 == 0x40:
			vals = append(vals, c^0x40)
		default:
			return nil, i, zScanBad
		}
	}
	return vals, i, zScanFound
}

func isXonXoff(c byte) bool {
	return c&0x7f == asciiXON || c&0x7f == asciiXOFF
}

// pollHeader returns a header that has already arrived, without waiting.
func (l *zLink) pollHeader() (zHeader, bool, error) {
	if l.held != nil {
		h := *l.held
		l.held = nil
		return h, true, nil
	}
	if err := l.poll(); err != nil {
		return zHeader{}, false, err
	}
	for {
		h, start, end, st := zScanHeader(l.pending, l.ctlIgnore)
		l.drop(start)
		end -= start
		switch st {
		case zScanFound:
			l.pending = l.pending[end:]
			return h, true, nil
		case zScanCancel:
			l.pending = l.pending[end:]
			l.gone = true
			return zHeader{}, false, errZPeerCancel
		case zScanBad:
			l.pending = l.pending[end:]
			continue
		default:
			return zHeader{}, false, nil
		}
	}
}

// readHeader waits up to wait for the next header. Garbage between frames
// is skipped. A header with a bad CRC returns errZBadHeader.
func (l *zLink) readHeader(wait time.Duration) (zHeader, error) {
	if l.held != nil {
		h := *l.held
		l.held = nil
		return h, nil
	}
	deadline := time.Now().Add(wait)
	for {
		h, start, end, st := zScanHeader(l.pending, l.ctlIgnore)
		l.drop(start)
		end -= start
		switch st {
		case zScanFound:
			l.pending = l.pending[end:]
			return h, nil
		case zScanCancel:
			l.pending = l.pending[end:]
			l.gone = true
			return zHeader{}, errZPeerCancel
		case zScanBad:
			l.pending = l.pending[end:]
			return zHeader{}, errZBadHeader
		}
		if err := l.fill(deadline); err != nil {
			return zHeader{}, err
		}
	}
}

// zdlread returns one decoded data byte, or 0x100|end for a frame end.
func (l *zLink) zdlread(deadline time.Time) (int, error) {
	for {
		c, err := l.readByte(deadline)
		if err != nil {
			return 0, err
		}
		if c&0x60 != 0 {
			return int(c), nil
		}
		if c != zDLE {
			if isXonXoff(c) || l.ctlIgnore {
				continue
			}
			return int(c), nil
		}
		break
	}
	cans := 1
	for {
		c, err := l.readByte(deadline)
		if err != nil {
			return 0, err
		}
		if c == asciiCAN {
			cans++
			if cans >= 5 {
				l.gone = true
				return 0, errZPeerCancel
			}
			continue
		}
		if cans > 1 {
			return 0, errZBadCRC
		}
		switch {
		case c == zCRCE || c == zCRCG || c == zCRCQ || c == zCRCW:
			return 0x100 | int(c), nil
		case c == zRUB0:
			return 0x7f, nil
		case c == zRUB1:
			return 0xff, nil
		case isXonXoff(c) || (l.ctlIgnore && c&0x60 == 0):
			continue
		case c&0x60 == 0x40:
			return int(c ^ 0x40), nil
		}
		return 0, errZBadCRC
	}
}

// readSubpacket reads one data subpacket of at most max bytes and checks
// its CRC. It returns the data and the frame end.
func (l *zLink) readSubpacket(buf []byte, max int, crc32Mode bool, wait time.Duration) ([]byte, byte, error) {
	deadline := time.Now().Add(wait)
	data := buf[:0]
	for {
		v, err := l.zdlread(deadline)
		if err != nil {
			return nil, 0, err
		}
		if v&0x100 == 0 {
			if len(data) >= max {
				return nil, 0, errZOverflow
			}
			data = append(data, byte(v))
			continue
		}
		end := byte(v)
		n := 2
		if crc32Mode {
			n = 4
		}
		var crcBytes [4]byte
		for i := 0; i < n; i++ {
			c, err := l.zdlread(deadline)
			if err != nil {
				return nil, 0, err
			}
			if c&0x100 != 0 {
				return nil, 0, errZUnexpectedFE
			}
			crcBytes[i] = byte(c)
		}
		if crc32Mode {
			crc := crc32.Update(crc32.ChecksumIEEE(data), crc32.IEEETable, []byte{end})
			if crc != binary.LittleEndian.Uint32(crcBytes[:4]) {
				return nil, 0, errZBadCRC
			}
		} else {
			crc := crc16Update(crc16XModem(data), end)
			if crc != uint16(crcBytes[0])<<8|uint16(crcBytes[1]) {
				return nil, 0, errZBadCRC
			}
		}
		return data, end, nil
	}
}

// unread puts one header back; the next read returns it first.
func (l *zLink) unread(h zHeader) {
	l.held = &h
}

// sendHex writes a hex header.
func (l *zLink) sendHex(h zHeader) error {
	return l.write(encodeHexHeader(h))
}

// sendCancel writes the cancel sequence.
func (l *zLink) sendCancel() {
	_ = l.write(zCancelSequence)
}

// drainText reads for up to max, until the line has been quiet for quiet,
// and echoes the device text lines it sees. An unfinished last line, such
// as the device prompt, is written as it is.
func (l *zLink) drainText(out io.Writer, quiet, max time.Duration) {
	saved := l.echo
	l.echo = out
	defer func() { l.echo = saved }()
	end := time.Now().Add(max)
	for time.Now().Before(end) {
		if len(l.pending) > 0 {
			l.drop(len(l.pending))
		}
		deadline := time.Now().Add(quiet)
		if deadline.After(end) {
			deadline = end
		}
		if err := l.fill(deadline); err != nil {
			break
		}
	}
	if len(l.pending) > 0 {
		l.drop(len(l.pending))
	}
	if tail := string(l.text); isDeviceTextLine(cleanDeviceLine(tail)) {
		_, _ = io.WriteString(out, tail)
	}
	l.text = l.text[:0]
}

func cleanDeviceLine(line string) string {
	b := make([]byte, 0, len(line))
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == 0x1b {
			i++
			if i < len(line) && line[i] == '[' {
				start := i + 1
				for i+1 < len(line) && (line[i+1] < 0x40 || line[i+1] > 0x7e) {
					i++
				}
				i++
				// Cursor-left (CSI n D) erases the n bytes before it: the
				// device shell erases its prompt that way before printing.
				if i < len(line) && line[i] == 'D' {
					n := 0
					for _, d := range line[start:i] {
						if d < '0' || d > '9' {
							n = -1
							break
						}
						n = n*10 + int(d-'0')
					}
					if n <= 0 {
						n = 1
					}
					b = b[:max(0, len(b)-n)]
				}
			}
			continue
		}
		b = append(b, c)
	}
	return strings.TrimSpace(string(b))
}
