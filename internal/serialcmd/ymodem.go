package serialcmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	ymodemSOH byte = 0x01
	ymodemSTX byte = 0x02
	ymodemEOT byte = 0x04
	ymodemACK byte = 0x06
	ymodemNAK byte = 0x15
	ymodemCAN byte = 0x18
	ymodemC   byte = 0x43

	ymodemEOFPad = 0x1A

	ymodemShortBlock = 128
	ymodemLongBlock  = 1024

	defaultYModemReadyWait    = 30 * time.Second
	defaultYModemPacketWait   = 3 * time.Second
	defaultYModemHandshakeMax = 30 * time.Second
	ymodemResultWait          = 5 * time.Second
	ymodemClaimWait           = 3 * time.Second
	ymodemPurgeQuiet          = 100 * time.Millisecond
	ymodemPurgeMax            = time.Second
	ymodemPollSlice           = 200 * time.Millisecond
	ymodemCInterval           = time.Second
	ymodemMaxPacketErrors     = 5
)

// YModemOptions drives one YMODEM-CRC file transfer on an already-open
// byte link. Address, when set, is dialed as a session control channel.
// Link is the test seam; production callers leave it nil.
type YModemOptions struct {
	Address    string
	Link       io.ReadWriter
	Command    string
	SendFile   string
	Payload    []byte
	RecvWriter io.Writer
	Output     io.Writer
	Status     *YModemStatus
	ReadyWait  time.Duration
	PacketWait time.Duration
}

// YModemStatus reports which direction ran and the block 0 file name.
type YModemStatus struct {
	Direction string
	Name      string
}

// ErrDeviceWaitingForFile means the device is sending C and this transfer has
// no local file to offer.
var ErrDeviceWaitingForFile = errors.New("device is waiting to receive a file; use sio sz <session> <file>")

var errYModemTimeout = errors.New("timed out")

// ymodemDeviceResult means the device printed its result line, so its side
// of the transfer has already ended.
type ymodemDeviceResult struct{ line string }

func (e *ymodemDeviceResult) Error() string {
	return "device ended the transfer: " + e.line
}

type ymodemConn struct {
	io.ReadWriter
	deadline time.Time
}

func (c *ymodemConn) SetReadDeadline(t time.Time) error {
	c.deadline = t
	if d, ok := c.ReadWriter.(interface{ SetReadDeadline(time.Time) error }); ok {
		return d.SetReadDeadline(t)
	}
	return nil
}

// ymodemLink keeps bytes read ahead of the current protocol step, so no
// byte is lost when one step hands over to the next.
type ymodemLink struct {
	conn    *ymodemConn
	pending []byte
	scratch []byte
	output  io.Writer
	// text collects device text seen between protocol bytes.
	text []byte
}

func (l *ymodemLink) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(l.output, "ymodem: "+format+"\n", args...)
}

// ymodemProgress prints transfer progress at most once a second, with the
// rate over the last interval, the average rate and the time left.
type ymodemProgress struct {
	l        *ymodemLink
	verb     string
	total    int
	start    time.Time
	last     time.Time
	lastDone int
}

func newYModemProgress(l *ymodemLink, verb string, total int) *ymodemProgress {
	now := time.Now()
	return &ymodemProgress{l: l, verb: verb, total: total, start: now, last: now}
}

func (p *ymodemProgress) update(done int) {
	now := time.Now()
	if now.Sub(p.last) < time.Second {
		return
	}
	cur := ymodemRate(done-p.lastDone, now.Sub(p.last))
	avg := ymodemRate(done, now.Sub(p.start))
	p.last = now
	p.lastDone = done
	if p.total > 0 {
		eta := "?"
		if avg > 0 {
			left := float64(p.total-done) / 1024 / avg
			eta = (time.Duration(left * float64(time.Second))).Round(time.Second).String()
		}
		p.l.logf("%s %d/%d bytes (%d%%) %.1f KB/s, avg %.1f KB/s, eta %s",
			p.verb, done, p.total, done*100/p.total, cur, avg, eta)
	} else {
		p.l.logf("%s %d bytes %.1f KB/s, avg %.1f KB/s", p.verb, done, cur, avg)
	}
}

func ymodemRate(bytes int, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) / 1024 / d.Seconds()
}

func (p *ymodemProgress) finish(done int) {
	elapsed := time.Since(p.start)
	rate := ymodemRate(done, elapsed)
	p.l.logf("%s %d bytes in %s (%.1f KB/s)", p.verb, done, elapsed.Round(time.Millisecond), rate)
}

// fill reads once and waits at most wait. It reports whether bytes arrived.
func (l *ymodemLink) fill(wait time.Duration) (bool, error) {
	if wait <= 0 {
		wait = time.Millisecond
	}
	_ = l.conn.SetReadDeadline(time.Now().Add(wait))
	n, err := l.conn.Read(l.scratch)
	if n > 0 {
		l.pending = append(l.pending, l.scratch[:n]...)
		return true, nil
	}
	if err != nil && !isTimeout(err) {
		return false, err
	}
	return false, nil
}

func (l *ymodemLink) write(data []byte) error {
	for len(data) > 0 {
		n, err := l.conn.Write(data)
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("ymodem short write")
		}
	}
	return nil
}

func (l *ymodemLink) writeByte(b byte) error {
	return l.write([]byte{b})
}

// readControl returns the next protocol byte. Other bytes are device text:
// each line is echoed, and a result line means the device side has ended.
func (l *ymodemLink) readControl(wait time.Duration) (byte, error) {
	deadline := time.Now().Add(wait)
	for {
		for len(l.pending) > 0 {
			b := l.pending[0]
			l.pending = l.pending[1:]
			switch b {
			case ymodemACK, ymodemNAK, ymodemCAN, ymodemC, ymodemEOT, ymodemSOH, ymodemSTX:
				return b, nil
			case '\r', '\n':
				if err := l.flushText(); err != nil {
					return 0, err
				}
			default:
				if len(l.text) < 512 {
					l.text = append(l.text, b)
				}
			}
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return 0, errYModemTimeout
		}
		if _, err := l.fill(minDuration(remain, ymodemPollSlice)); err != nil {
			return 0, err
		}
	}
}

func (l *ymodemLink) flushText() error {
	line := cleanYModemLine(string(l.text))
	l.text = l.text[:0]
	if line == "" {
		return nil
	}
	_, _ = fmt.Fprintln(l.output, line)
	if isYModemResult(line) {
		return &ymodemDeviceResult{line: line}
	}
	return nil
}

// waitControl waits for want and skips every other protocol byte except CAN.
func (l *ymodemLink) waitControl(want byte, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		got, err := l.readControl(time.Until(deadline))
		if err != nil {
			return fmt.Errorf("waiting %s for %s: %w", wait, ymodemByteName(want), err)
		}
		if got == want {
			return nil
		}
		if got == ymodemCAN {
			return errYModemCancelled
		}
	}
}

// purge drops input until the line has been quiet for ymodemPurgeQuiet,
// so the next header read starts on a packet boundary.
func (l *ymodemLink) purge() {
	deadline := time.Now().Add(ymodemPurgeMax)
	for time.Now().Before(deadline) {
		l.pending = nil
		got, err := l.fill(ymodemPurgeQuiet)
		if err != nil || !got {
			return
		}
	}
	l.pending = nil
}

func (l *ymodemLink) readExact(n int, wait time.Duration) ([]byte, error) {
	deadline := time.Now().Add(wait)
	for len(l.pending) < n {
		remain := time.Until(deadline)
		if remain <= 0 {
			return nil, errYModemTimeout
		}
		if _, err := l.fill(minDuration(remain, ymodemPollSlice)); err != nil {
			return nil, err
		}
	}
	out := append([]byte(nil), l.pending[:n]...)
	l.pending = l.pending[n:]
	return out, nil
}

// takeLines consumes complete text lines in pending[:limit] and echoes them.
// It stops at "ready" or a result line and returns that line.
func (l *ymodemLink) takeLines(limit int) string {
	for {
		region := l.pending[:limit]
		line, rest, ok := nextTextLine(region)
		if !ok {
			return ""
		}
		used := len(region) - len(rest)
		l.pending = l.pending[used:]
		limit -= used
		text := cleanYModemLine(line)
		if text == "" {
			continue
		}
		_, _ = fmt.Fprintln(l.output, text)
		if text == "ready" || isYModemResult(text) {
			return text
		}
	}
}

// TransferYModem runs one YMODEM-CRC file on an already-open link. It writes
// only protocol bytes. The device shell command is entered by the user.
func TransferYModem(opts YModemOptions) error {
	canSend := opts.SendFile != "" || opts.Payload != nil
	canReceive := opts.RecvWriter != nil
	if canSend == canReceive {
		return errors.New("ymodem transfer needs exactly one direction")
	}
	if opts.ReadyWait <= 0 {
		opts.ReadyWait = defaultYModemReadyWait
	}
	if opts.PacketWait <= 0 {
		opts.PacketWait = defaultYModemPacketWait
	}
	output := opts.Output
	if output == nil {
		output = io.Discard
	}

	var conn io.ReadWriter
	if opts.Link != nil {
		conn = opts.Link
	} else {
		if opts.Address == "" {
			return errors.New("ymodem address is required")
		}
		dialed, err := net.Dial("tcp", opts.Address)
		if err != nil {
			return err
		}
		defer dialed.Close()
		conn = dialed
	}
	link := &ymodemLink{
		conn:    &ymodemConn{ReadWriter: conn},
		scratch: make([]byte, 4096),
		output:  output,
	}

	if err := claimPort(link, ymodemClaimWait); err != nil {
		return err
	}
	link.logf("claimed the session port")

	var err error
	if canSend {
		if opts.Status != nil {
			opts.Status.Direction = "send"
		}
		err = ymodemSendFile(link, opts.SendFile, opts.Payload, opts.ReadyWait, opts.PacketWait)
	} else {
		if opts.Status != nil {
			opts.Status.Direction = "recv"
		}
		var name string
		name, err = ymodemReceiveFile(link, opts.RecvWriter, opts.ReadyWait, opts.PacketWait)
		if err == nil && opts.Status != nil {
			opts.Status.Name = name
		}
	}
	var ended *ymodemDeviceResult
	if errors.As(err, &ended) {
		return ymodemResultError(ended.line)
	}
	if errors.Is(err, errYModemCancelled) {
		// The device cancels first and prints the reason, such as exists,
		// once its console is back.
		if result, resErr := readResultLine(link, ymodemResultWait); resErr == nil {
			return ymodemResultError(result)
		}
	}
	if err != nil {
		return err
	}

	result, err := readResultLine(link, ymodemResultWait)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(result, "ok ") {
		return ymodemResultError(result)
	}
	return nil
}

// ErrYModemPortBusy means another transfer already holds the session port.
var ErrYModemPortBusy = errors.New("another ymodem transfer holds the session port")

// claimPort asks the session worker for exclusive writes. The answer token
// arrives in line with serial output; bytes around it stay in pending.
func claimPort(l *ymodemLink, wait time.Duration) error {
	if err := l.write([]byte(ymodemClaimPreamble)); err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for {
		if idx := bytes.Index(l.pending, []byte(ymodemClaimOK)); idx >= 0 {
			l.pending = append(l.pending[:idx:idx], l.pending[idx+len(ymodemClaimOK):]...)
			return nil
		}
		if bytes.Contains(l.pending, []byte(ymodemClaimBusy)) {
			return ErrYModemPortBusy
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return errors.New("session worker did not answer the ymodem claim; restart it with sio stop and sio open")
		}
		if _, err := l.fill(minDuration(remain, ymodemPollSlice)); err != nil {
			return err
		}
	}
}

// waitForReceiverC waits until the device runs its receive command. Device
// text is echoed. "ready" or the device C starts the transfer. A C counts
// only as a run of bare C bytes on its own line followed by a quiet read, so
// log text such as "CDC" cannot start it.
func waitForReceiverC(l *ymodemLink, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	quiet := false
	for {
		if text := l.takeLines(len(l.pending)); text != "" {
			if text == "ready" {
				return nil
			}
			return ymodemResultError(text)
		}
		if len(l.pending) > 0 && quiet && isOnlyC(l.pending) {
			l.pending = nil
			return nil
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			writeWaitingText(l.output, l.pending)
			return fmt.Errorf("ymodem ready timeout after %s", wait)
		}
		got, err := l.fill(minDuration(remain, ymodemPollSlice))
		if err != nil {
			return err
		}
		quiet = !got
	}
}

func ymodemSendFile(l *ymodemLink, name string, payload []byte, readyWait, packetWait time.Duration) error {
	l.logf("waiting up to %s for the device receiver (rz)", readyWait)
	if err := waitForReceiverC(l, readyWait); err != nil {
		return err
	}
	base := path.Base(strings.ReplaceAll(name, "\\", "/"))
	block0, err := encodeYModemData(0, ymodemBlock0Body(base, len(payload)), ymodemSOH)
	if err != nil {
		return err
	}
	l.logf("device is ready; sending block 0 name=%q size=%d", base, len(payload))
	if err := sendBlock0(l, block0, packetWait); err != nil {
		return err
	}
	progress := newYModemProgress(l, "sent", len(payload))

	seq := byte(1)
	for offset := 0; offset < len(payload); {
		header := ymodemSTX
		size := ymodemLongBlock
		if len(payload)-offset <= ymodemShortBlock {
			header = ymodemSOH
			size = ymodemShortBlock
		}
		body := bytes.Repeat([]byte{ymodemEOFPad}, size)
		n := copy(body, payload[offset:])
		frame, err := encodeYModemData(seq, body, header)
		if err != nil {
			return err
		}
		if err := sendFrame(l, frame, packetWait, fmt.Sprintf("block %d", seq)); err != nil {
			return fmt.Errorf("ymodem block %d: %w", seq, err)
		}
		offset += n
		seq++
		progress.update(offset)
	}
	if err := finishYModemSend(l, packetWait); err != nil {
		return err
	}
	progress.finish(len(payload))
	return nil
}

// sendBlock0 sends the header block on the device C. The device keeps
// sending C until block 0 arrives, so a C before the ACK is stale. After the
// ACK the device sends one more C to ask for data.
func sendBlock0(l *ymodemLink, frame []byte, wait time.Duration) error {
	if err := l.waitControl(ymodemC, defaultYModemHandshakeMax); err != nil {
		return fmt.Errorf("ymodem handshake: %w", err)
	}
	if err := sendFrame(l, frame, wait, "block 0"); err != nil {
		return fmt.Errorf("ymodem block 0: %w", err)
	}
	if err := l.waitControl(ymodemC, wait); err != nil {
		return fmt.Errorf("ymodem block 0 accepted, then %w", err)
	}
	return nil
}

// sendFrame writes one packet and waits for its ACK. NAK or silence resends
// the packet. A C while waiting is a stale handshake byte and is skipped.
func sendFrame(l *ymodemLink, frame []byte, wait time.Duration, label string) error {
	var last error
	for attempt := 1; attempt <= ymodemMaxPacketErrors; attempt++ {
		if err := l.write(frame); err != nil {
			return err
		}
		last = waitACK(l, wait)
		if last == nil {
			return nil
		}
		var ended *ymodemDeviceResult
		if errors.Is(last, errYModemCancelled) || errors.As(last, &ended) {
			return last
		}
		if attempt < ymodemMaxPacketErrors {
			l.logf("%s: %v; resending (%d/%d)", label, last, attempt+1, ymodemMaxPacketErrors)
		}
	}
	return fmt.Errorf("no ACK after %d tries (last: %w)", ymodemMaxPacketErrors, last)
}

var errYModemCancelled = errors.New("ymodem cancelled by peer")

func waitACK(l *ymodemLink, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		got, err := l.readControl(time.Until(deadline))
		if errors.Is(err, errYModemTimeout) {
			return fmt.Errorf("no answer within %s", wait)
		}
		if err != nil {
			return err
		}
		switch got {
		case ymodemACK:
			return nil
		case ymodemNAK:
			return errors.New("rejected with NAK")
		case ymodemCAN:
			return errYModemCancelled
		}
	}
}

func finishYModemSend(l *ymodemLink, wait time.Duration) error {
	acked := false
	for attempt := 0; attempt < ymodemMaxPacketErrors && !acked; attempt++ {
		if err := l.writeByte(ymodemEOT); err != nil {
			return err
		}
		got, err := l.readControl(wait)
		var ended *ymodemDeviceResult
		if errors.As(err, &ended) {
			return err
		}
		if err != nil {
			l.logf("EOT: %v; resending", err)
			continue
		}
		switch got {
		case ymodemACK:
			acked = true
		case ymodemCAN:
			return errYModemCancelled
		}
		// NAK asks for the second EOT.
	}
	if !acked {
		return errors.New("ymodem eot was not acknowledged")
	}
	if err := l.waitControl(ymodemC, wait); err != nil {
		return fmt.Errorf("ymodem closing handshake: %w", err)
	}
	empty, err := encodeYModemData(0, ymodemBlock0Body("", 0), ymodemSOH)
	if err != nil {
		return err
	}
	return sendFrame(l, empty, wait, "closing block 0")
}

// waitForBlock0 sends C every second until the device answers with block 0.
// The device command is normally already running, so its "ready" line may be
// gone. Text lines are echoed; a result line ends the wait with an error.
func waitForBlock0(l *ymodemLink, wait time.Duration) ([]byte, error) {
	const frameLen = 3 + ymodemShortBlock + 2
	deadline := time.Now().Add(wait)
	nextC := time.Now()
	quiet := false
	for {
		if !time.Now().Before(nextC) {
			if err := l.writeByte(ymodemC); err != nil {
				return nil, err
			}
			nextC = time.Now().Add(ymodemCInterval)
		}
		limit := len(l.pending)
		if idx := bytes.IndexByte(l.pending, ymodemSOH); idx >= 0 {
			limit = idx
		}
		if text := l.takeLines(limit); text != "" && text != "ready" {
			return nil, ymodemResultError(text)
		}
		if idx := bytes.IndexByte(l.pending, ymodemSOH); idx >= 0 && len(l.pending)-idx >= frameLen {
			frame := l.pending[idx : idx+frameLen]
			body := frame[3 : 3+ymodemShortBlock]
			crc := uint16(frame[3+ymodemShortBlock])<<8 | uint16(frame[4+ymodemShortBlock])
			if frame[1] == 0 && frame[2] == 0xFF && crc16XModem(body) == crc {
				out := append([]byte(nil), body...)
				l.pending = l.pending[idx+frameLen:]
				return out, nil
			}
			l.pending = l.pending[idx+1:]
			continue
		}
		if len(l.pending) > 0 && quiet && isOnlyC(l.pending) {
			return nil, ErrDeviceWaitingForFile
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			writeWaitingText(l.output, l.pending)
			return nil, fmt.Errorf("ymodem ready timeout after %s", wait)
		}
		got, err := l.fill(minDuration(minDuration(remain, time.Until(nextC)), ymodemPollSlice))
		if err != nil {
			return nil, err
		}
		quiet = !got
	}
}

func ymodemReceiveFile(l *ymodemLink, dst io.Writer, readyWait, packetWait time.Duration) (string, error) {
	l.logf("sending C every second for up to %s; waiting for the device sender (sz)", readyWait)
	block0, err := waitForBlock0(l, readyWait)
	if err != nil {
		return "", err
	}
	name, size, err := parseYModemBlock0(block0)
	if err != nil {
		_ = l.write([]byte{ymodemCAN, ymodemCAN})
		return "", err
	}
	if name == "" {
		_ = l.writeByte(ymodemACK)
		return "", errors.New("ymodem sender has no file")
	}
	if err := l.write([]byte{ymodemACK, ymodemC}); err != nil {
		return "", err
	}
	l.logf("receiving name=%q size=%d", name, size)
	progress := newYModemProgress(l, "received", size)

	got := 0
	expect := byte(1)
	errorsLeft := ymodemMaxPacketErrors
	for {
		header, err := l.readControl(packetWait)
		if err == nil && header == ymodemEOT {
			break
		}
		if err == nil && header == ymodemCAN {
			return "", errYModemCancelled
		}
		var seq byte
		var body []byte
		if err == nil && (header == ymodemSOH || header == ymodemSTX) {
			seq, body, err = readPacketBody(l, header, packetWait)
		} else if err == nil {
			err = fmt.Errorf("ymodem unexpected header %02x", header)
		}
		var ended *ymodemDeviceResult
		if errors.As(err, &ended) {
			return "", err
		}
		if err != nil {
			errorsLeft--
			if errorsLeft <= 0 {
				_ = l.write([]byte{ymodemCAN, ymodemCAN})
				return "", fmt.Errorf("block %d: %w; giving up", expect, err)
			}
			l.logf("block %d: %v; NAK (%d tries left)", expect, err, errorsLeft)
			l.purge()
			if err := l.writeByte(ymodemNAK); err != nil {
				return "", err
			}
			continue
		}
		errorsLeft = ymodemMaxPacketErrors
		switch seq {
		case expect:
			take := len(body)
			if size >= 0 && take > size-got {
				take = size - got
			}
			if take > 0 {
				if _, err := dst.Write(body[:take]); err != nil {
					_ = l.write([]byte{ymodemCAN, ymodemCAN})
					return "", err
				}
				got += take
			}
			expect++
			progress.update(got)
		case expect - 1:
			// The sender missed our ACK and sent the same block again.
			l.logf("block %d repeated; ACK again", seq)
		default:
			_ = l.write([]byte{ymodemCAN, ymodemCAN})
			return "", fmt.Errorf("ymodem block %d out of order, want %d", seq, expect)
		}
		if err := l.writeByte(ymodemACK); err != nil {
			return "", err
		}
	}
	if size >= 0 && got != size {
		_ = l.write([]byte{ymodemCAN, ymodemCAN})
		return "", fmt.Errorf("ymodem short file: got %d of %d bytes", got, size)
	}
	if err := finishYModemReceive(l, packetWait); err != nil {
		return "", err
	}
	progress.finish(got)
	return name, nil
}

// readPacketBody reads seq, ~seq, data and CRC after the header byte.
func readPacketBody(l *ymodemLink, header byte, wait time.Duration) (byte, []byte, error) {
	size := ymodemShortBlock
	if header == ymodemSTX {
		size = ymodemLongBlock
	}
	rest, err := l.readExact(2+size+2, wait)
	if err != nil {
		return 0, nil, err
	}
	body := rest[2 : 2+size]
	crc := uint16(rest[2+size])<<8 | uint16(rest[3+size])
	if rest[0] != ^rest[1] || crc16XModem(body) != crc {
		return 0, nil, errors.New("ymodem crc")
	}
	return rest[0], body, nil
}

// finishYModemReceive answers the first EOT with NAK and the second with
// ACK, then asks for the empty block 0 that ends the batch.
func finishYModemReceive(l *ymodemLink, wait time.Duration) error {
	if err := l.writeByte(ymodemNAK); err != nil {
		return err
	}
	if err := l.waitControl(ymodemEOT, wait); err != nil {
		return fmt.Errorf("ymodem second eot: %w", err)
	}
	if err := l.write([]byte{ymodemACK, ymodemC}); err != nil {
		return err
	}
	if err := l.waitControl(ymodemSOH, wait); err != nil {
		return fmt.Errorf("ymodem closing block: %w", err)
	}
	if _, _, err := readPacketBody(l, ymodemSOH, wait); err != nil {
		return fmt.Errorf("ymodem closing block: %w", err)
	}
	return l.writeByte(ymodemACK)
}

// readResultLine waits for the device result line. Log lines printed after
// the device restores its console are echoed and skipped.
func readResultLine(l *ymodemLink, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		if text := l.takeLines(len(l.pending)); text != "" && text != "ready" {
			return text, nil
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return "", errors.New("ymodem result timeout")
		}
		if _, err := l.fill(minDuration(remain, ymodemPollSlice)); err != nil {
			return "", err
		}
	}
}

func writeWaitingText(output io.Writer, data []byte) {
	if len(data) == 0 || output == nil {
		return
	}
	text := strings.TrimRight(string(data), "\r\n")
	if text == "" {
		return
	}
	_, _ = fmt.Fprintln(output, text)
}

func ymodemByteName(b byte) string {
	switch b {
	case ymodemSOH:
		return "SOH"
	case ymodemSTX:
		return "STX"
	case ymodemEOT:
		return "EOT"
	case ymodemACK:
		return "ACK"
	case ymodemNAK:
		return "NAK"
	case ymodemCAN:
		return "CAN"
	case ymodemC:
		return "C"
	default:
		return fmt.Sprintf("%02x", b)
	}
}

func isOnlyC(data []byte) bool {
	for _, b := range data {
		if b != ymodemC {
			return false
		}
	}
	return len(data) > 0
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func nextTextLine(data []byte) (string, []byte, bool) {
	idx := bytes.IndexByte(data, '\n')
	if idx < 0 {
		idx = bytes.IndexByte(data, '\r')
		if idx < 0 {
			return "", data, false
		}
	}
	line := string(data[:idx])
	rest := data[idx+1:]
	if len(rest) > 0 && rest[0] == '\n' && data[idx] == '\r' {
		rest = rest[1:]
	}
	return line, rest, true
}

// cleanYModemLine drops VT100 escape sequences and keeps the text after the
// last carriage return. The device shell erases its prompt before printing.
// cleanYModemLine drops escape sequences from a device line. Cursor-left
// (CSI n D) also drops the n bytes before it: the device shell erases its
// prompt that way before printing, as in "dev$ [8D[Jexists".
func cleanYModemLine(line string) string {
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
				if i < len(line) && line[i] == 'D' {
					n, err := strconv.Atoi(line[start:i])
					if err != nil {
						n = 1
					}
					b = b[:max(0, len(b)-n)]
				}
			}
			continue
		}
		b = append(b, c)
	}
	text := string(b)
	if idx := strings.LastIndexByte(strings.TrimRight(text, "\r"), '\r'); idx >= 0 {
		text = text[idx+1:]
	}
	return strings.TrimSpace(text)
}

func isYModemResult(line string) bool {
	switch {
	case line == "timeout", line == "crc", line == "cancel", line == "io", line == "exists", line == "busy":
		return true
	case strings.HasPrefix(line, "ok "):
		return true
	default:
		return false
	}
}

func ymodemResultError(line string) error {
	return fmt.Errorf("ymodem result: %s", line)
}

func encodeYModemData(seq byte, body []byte, header byte) ([]byte, error) {
	size := ymodemShortBlock
	if header == ymodemSTX {
		size = ymodemLongBlock
	}
	if header != ymodemSOH && header != ymodemSTX {
		return nil, fmt.Errorf("ymodem header %02x", header)
	}
	if len(body) != size {
		return nil, fmt.Errorf("ymodem body length %d, want %d", len(body), size)
	}
	frame := make([]byte, 3+size+2)
	frame[0] = header
	frame[1] = seq
	frame[2] = ^seq
	copy(frame[3:], body)
	crc := crc16XModem(body)
	frame[3+size] = byte(crc >> 8)
	frame[4+size] = byte(crc)
	return frame, nil
}

func ymodemBlock0Body(name string, length int) []byte {
	body := make([]byte, ymodemShortBlock)
	prefix := name + "\x00"
	if name != "" || length > 0 {
		prefix = name + "\x00" + strconv.Itoa(length) + "\x00"
	}
	copy(body, prefix)
	return body
}

// parseYModemBlock0 reads "name\0size mtime mode ...". The size is -1 when
// the sender leaves it out. An empty name ends the batch.
func parseYModemBlock0(body []byte) (string, int, error) {
	nameEnd := bytes.IndexByte(body, 0)
	if nameEnd < 0 {
		return "", 0, errors.New("ymodem block 0 missing name")
	}
	name := string(body[:nameEnd])
	if name == "" {
		return "", 0, nil
	}
	rest := body[nameEnd+1:]
	if end := bytes.IndexByte(rest, 0); end >= 0 {
		rest = rest[:end]
	}
	if end := bytes.IndexByte(rest, ' '); end >= 0 {
		rest = rest[:end]
	}
	if len(rest) == 0 {
		return name, -1, nil
	}
	size, err := strconv.Atoi(string(rest))
	if err != nil || size < 0 {
		return "", 0, fmt.Errorf("ymodem block 0 length %q", rest)
	}
	return name, size, nil
}

func crc16XModem(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
