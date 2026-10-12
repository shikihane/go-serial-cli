package serialcmd

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultZHandshakeWait = 120 * time.Second
	defaultZDataWait      = 10 * time.Second
	defaultZWindow        = 32 * 1024
	defaultZSubpacket     = 1024
	zMinSubpacket         = 32
	zRecvSubpacketMax     = 8192
	zMaxRetries           = 20
	zGrowAfter            = 16
	zRQInitResend         = 10 * time.Second
	// ZRINIT is repeated on silence at the lrzsz interval. A shorter one
	// keeps restarting a sender's own wait for an answer to its offer.
	zRInitResend    = 10 * time.Second
	zFileResend     = 10 * time.Second
	zStaleRInitWait = 2 * time.Second
	// A peer may compute a CRC over a large file on a slow card before it
	// answers ZFILE or ZCRC; the wait grows with the file at this rate.
	zSlowCRCRate = 64 * 1024
	// An identical ZFILE this soon after it was answered was sent before
	// the answer reached the peer.
	zRepeatWindow = 3 * time.Second
	// A ZFILE repeated after our ZRPOS but before any data is answered
	// again only when it came this long after that ZRPOS.
	zRepeatGap      = 2 * time.Second
	zBadHeaderRInit = time.Second
	// A ZCRC request is asked this many times, like the device does.
	zCRCTries    = 3
	zAckPokeWait = 3 * time.Second
	zAckPokes    = 4
	zClaimWait   = 3 * time.Second
	zResultQuiet = 300 * time.Millisecond
	zResultMax   = 2 * time.Second
	// A ZRPOS for the start of the open frame that arrives within this of
	// the frame's first subpacket going out is too early to be about that
	// subpacket: the receiver cannot have read it and answered yet.
	zRposEcho = 2 * time.Millisecond
	// A sender held by XOFF goes on after this long without XON, as the
	// device does; the XON may be lost.
	zXoffWait = 10 * time.Second
)

// ConflictPolicy says what a receiver does when the target file exists.
type ConflictPolicy string

const (
	ConflictSkipIdenticalElseRename ConflictPolicy = "skip-identical-else-rename"
	ConflictOverwrite               ConflictPolicy = "overwrite"
	ConflictSkip                    ConflictPolicy = "skip"
	ConflictRename                  ConflictPolicy = "rename"
)

// ParseConflictPolicy accepts the policy names used on the command line.
func ParseConflictPolicy(value string) (ConflictPolicy, error) {
	switch p := ConflictPolicy(strings.ToLower(strings.TrimSpace(value))); p {
	case "":
		return ConflictSkipIdenticalElseRename, nil
	case ConflictSkipIdenticalElseRename, ConflictOverwrite, ConflictSkip, ConflictRename:
		return p, nil
	}
	return "", fmt.Errorf("unknown conflict policy %q; use skip-identical-else-rename, overwrite, skip or rename", value)
}

// zmanagement is the ZF1 option a sender asks for under a local policy.
func (p ConflictPolicy) zmanagement() byte {
	switch p {
	case ConflictOverwrite:
		return zMCLOB
	case ConflictSkip:
		return zMPROT
	case ConflictRename:
		return zMCHNG
	case zConflictNone:
		return 0
	}
	return zMCRC
}

// zConflictNone sends no management option, as lrzsz sz does by default.
// Tests use it to exercise the receiver's own policy.
const zConflictNone ConflictPolicy = "none"

// Transfer outcomes, from the best case to the worst.
const (
	OutcomeOK      = "ok"
	OutcomeResumed = "resumed"
	// OutcomeSkipped: the target already held the same content.
	OutcomeSkipped = "skipped"
	// OutcomeRefused: the receiver kept an existing file and did not take
	// this one. It never compared contents.
	OutcomeRefused   = "refused"
	OutcomeCancelled = "cancelled"
	OutcomeFailed    = "failed"
	// OutcomePartial: a batch where some files went through and others
	// were refused or failed.
	OutcomePartial = "partial"
)

var (
	// ErrTransferPortBusy means another transfer already holds the port.
	ErrTransferPortBusy = errors.New("another file transfer holds the session port")
	// ErrTransferCancelled means this side or the peer cancelled.
	ErrTransferCancelled = errors.New("transfer cancelled")
)

// ZSendFile is one file to send. Data is used when Path is empty.
type ZSendFile struct {
	Name    string
	Path    string
	Data    []byte
	ModTime time.Time
}

// ZFileInfo is what the sender said about a file in ZFILE.
type ZFileInfo struct {
	Name      string
	RawName   string
	Size      int64
	ModTime   time.Time
	Mode      uint32
	FilesLeft int
	BytesLeft int64
	Index     int
}

// ZDestination is where a received file goes. Policy, when set, replaces
// the transfer's conflict policy for this file.
type ZDestination struct {
	Path   string
	Policy ConflictPolicy
}

// ZFileResult reports one file.
type ZFileResult struct {
	Name         string
	Path         string
	Size         int64
	Bytes        int64
	Status       string
	Reason       string
	ResumeFrom   int64
	Retransmits  int
	CRCErrors    int
	WindowBlocks int
	MinBlock     int
	MaxBlock     int
	Elapsed      time.Duration
}

// ZModemResult reports a whole session.
type ZModemResult struct {
	Direction  string
	Outcome    string
	Files      []ZFileResult
	PeerBuffer int
	Window     int
	CRC32      bool
	EscapeCtl  bool
	FullDuplex bool
}

// ZProgress is passed to the progress callback on the transfer goroutine:
// when the session changes phase, after every data subpacket and when a
// retransmission or CRC error is counted.
type ZProgress struct {
	// Send is set when this side sends the files.
	Send  bool
	Phase string
	Name  string
	Done  int64
	// Total is -1 when the sender gave no size.
	Total      int64
	ResumeFrom int64
	// File counts from 1; Files is 0 while the batch size is unknown.
	File        int
	Files       int
	Retransmits int
	CRCErrors   int
	// Window is the sender's window W, or the receive buffer this side
	// offered; 0 means none.
	Window int
	CRC32  bool
}

// Session phases in ZProgress.
const (
	// ZPhaseHandshake: waiting for the peer to start the session.
	ZPhaseHandshake = "handshake"
	// ZPhasePick: a person is choosing files or a destination.
	ZPhasePick = "pick"
	// ZPhaseOffer: a file was offered and waits for an answer.
	ZPhaseOffer = "offer"
	// ZPhaseCheck: a ZCRC compares an existing file with the offered one.
	ZPhaseCheck  = "check"
	ZPhaseData   = "data"
	ZPhaseFinish = "finish"
)

// ZModemOptions drives one ZMODEM session. Address, when set, is dialed as
// a session control channel and claimed for the transfer. Link is the test
// seam; it is used as is, without a claim.
type ZModemOptions struct {
	Address string
	Link    io.ReadWriter

	// Receive selects the receiving side. Otherwise Files are sent, or
	// PickFiles is called after the claim to choose them.
	Receive   bool
	Files     []ZSendFile
	PickFiles func() ([]ZSendFile, error)

	// Destination picks the path of each received file. When nil, files go
	// to DestDir under their own name, and the first one to DestPath when
	// that is set.
	Destination func(info ZFileInfo) (ZDestination, error)
	DestDir     string
	DestPath    string

	// Conflict, when set, is the user's explicit policy and wins over the
	// management option a sender puts in ZF1. Empty obeys the sender's
	// option, else skip-identical-else-rename. A sender maps it to ZF1.
	Conflict     ConflictPolicy
	Window       int
	RecvBuffer   int
	MaxSubpacket int
	EscapeCtl    bool
	NoResume     bool

	HandshakeWait time.Duration
	DataWait      time.Duration

	Output io.Writer
	// DeviceText receives the device's text lines around the session;
	// nil means Output.
	DeviceText     io.Writer
	ProgressInline bool
	// Progress, when set, replaces the progress line on Output.
	Progress func(ZProgress)
	Cancel   <-chan struct{}
	// Force, when closed after Cancel, also skips the wait for the device
	// result line: a second Ctrl+C.
	Force  <-chan struct{}
	Result *ZModemResult
	// ClosePicker is called when the peer gives up, or Cancel fires, while
	// PickFiles or Destination is still waiting for a person. It should
	// make that call return.
	ClosePicker func()

	// Test knobs: a receiver without CANFDX and a sender that ignores ZCRC.
	halfDuplex bool
	ignoreZCRC bool
	noSize     bool
	crcWait    time.Duration
	noDrain    bool
	// trace sees the sender position and acknowledged offset per subpacket.
	trace func(pos, acked int64)
	// sourceHook may replace a file's reader, to inject read errors.
	sourceHook func(name string, src *zSource)
	// xoffWait replaces zXoffWait.
	xoffWait time.Duration
}

// TransferZModem runs one ZMODEM session, sending or receiving any number
// of files. It returns nil when every file was sent, received or skipped.
func TransferZModem(opts ZModemOptions) error {
	if opts.HandshakeWait <= 0 {
		opts.HandshakeWait = defaultZHandshakeWait
	}
	if opts.DataWait <= 0 {
		opts.DataWait = defaultZDataWait
	}
	if opts.Window <= 0 {
		opts.Window = defaultZWindow
	}
	if opts.MaxSubpacket <= 0 || opts.MaxSubpacket > defaultZSubpacket {
		opts.MaxSubpacket = defaultZSubpacket
	}
	if opts.MaxSubpacket < zMinSubpacket {
		opts.MaxSubpacket = zMinSubpacket
	}
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	result := opts.Result
	if result == nil {
		result = &ZModemResult{}
	}
	*result = ZModemResult{Direction: "send"}
	if opts.Receive {
		result.Direction = "recv"
	}

	conn := opts.Link
	if conn == nil {
		if opts.Address == "" {
			return errors.New("zmodem needs a session address")
		}
		dialed, err := net.Dial("tcp", opts.Address)
		if err != nil {
			return err
		}
		defer dialed.Close()
		conn = dialed
	}
	s := &zSession{opts: &opts, res: result, out: opts.Output, text: opts.DeviceText}
	if s.text == nil {
		s.text = opts.Output
	}
	s.l = newZLink(conn, opts.Cancel)
	defer s.l.close()
	if opts.Link == nil {
		if err := claimTransferPort(s.l, zClaimWait); err != nil {
			result.Outcome = OutcomeFailed
			return err
		}
	}

	var err error
	if opts.Receive {
		err = s.receive()
	} else {
		err = s.send()
	}
	return s.finish(err)
}

type zSession struct {
	l    *zLink
	opts *ZModemOptions
	res  *ZModemResult
	out  io.Writer
	text io.Writer

	crc32      bool
	esc        zEscaper
	fullDuplex bool
	window     int
	maxBlock   int
	attn       []byte
	progress   *zProgressLine
	// live is what the progress callback sees, liveRes the result of the
	// file it describes.
	live    ZProgress
	liveRes *ZFileResult
}

// report passes the current state to the progress callback, after moving
// to phase unless that is empty.
func (s *zSession) report(phase string) {
	if phase != "" {
		s.live.Phase = phase
	}
	if s.opts.Progress == nil {
		return
	}
	p := s.live
	if r := s.liveRes; r != nil {
		p.Retransmits, p.CRCErrors = r.Retransmits, r.CRCErrors
	}
	s.opts.Progress(p)
}

// reportFile starts the progress of one offered file.
func (s *zSession) reportFile(phase, name string, total int64, file int, res *ZFileResult) {
	s.live.Name, s.live.Total, s.live.File = name, total, file
	s.live.Done, s.live.ResumeFrom = 0, 0
	s.liveRes = res
	s.report(phase)
}

func (s *zSession) logf(format string, args ...any) {
	s.endProgress()
	_, _ = fmt.Fprintf(s.out, "zmodem: "+format+"\n", args...)
}

// finish sends the cancel sequence when needed, shows the device text that
// follows the session and sets the outcome.
func (s *zSession) finish(err error) error {
	s.endProgress()
	peer := errors.Is(err, errZPeerCancel) || errors.Is(err, errZPeerAbort)
	cancelled := peer || errors.Is(err, errZLocalCancel) || errors.Is(err, ErrTransferCancelled)
	if err != nil && !s.l.gone && !errors.Is(err, errZLinkClosed) {
		// The peer may still be waiting; tell it to stop.
		s.l.sendCancel()
	}
	s.report(ZPhaseFinish)
	if !errors.Is(err, errZLinkClosed) && !s.opts.noDrain {
		// The device prints a result line once its console is back.
		s.l.cancel = s.opts.Force
		s.l.drainText(s.text, zResultQuiet, zResultMax)
	}
	switch {
	case cancelled:
		s.res.Outcome = OutcomeCancelled
		if peer {
			return fmt.Errorf("%w by the peer", ErrTransferCancelled)
		}
		return ErrTransferCancelled
	case err != nil:
		s.res.Outcome = OutcomeFailed
		return err
	}
	s.res.Outcome = sessionOutcome(s.res.Files)
	switch s.res.Outcome {
	case OutcomeFailed, OutcomePartial, OutcomeRefused:
		var names []string
		for _, f := range s.res.Files {
			if f.Status == OutcomeFailed || f.Status == OutcomeRefused {
				names = append(names, f.Name+" ("+f.Status+": "+f.Reason+")")
			}
		}
		return fmt.Errorf("%d of %d file(s) not transferred: %s", len(names), len(s.res.Files), strings.Join(names, ", "))
	}
	return nil
}

// sessionOutcome sums up a batch. Every file transferred, resumed or
// already identical counts as done; any refused or failed file makes the
// batch partial (or wholly refused or failed), never plain ok.
func sessionOutcome(files []ZFileResult) string {
	if len(files) == 0 {
		return OutcomeOK
	}
	var ok, resumed, skipped, refused, failed int
	for _, f := range files {
		switch f.Status {
		case OutcomeOK:
			ok++
		case OutcomeResumed:
			resumed++
		case OutcomeSkipped:
			skipped++
		case OutcomeRefused:
			refused++
		default:
			failed++
		}
	}
	switch {
	case refused+failed == 0 && resumed > 0:
		return OutcomeResumed
	case refused+failed == 0 && ok > 0:
		return OutcomeOK
	case refused+failed == 0:
		return OutcomeSkipped
	case ok+resumed+skipped > 0:
		return OutcomePartial
	case failed == 0:
		return OutcomeRefused
	}
	return OutcomeFailed
}

// claimTransferPort asks the session worker for exclusive writes. The
// answer token arrives in line with serial output; bytes around it stay.
func claimTransferPort(l *zLink, wait time.Duration) error {
	if err := l.write([]byte(transferClaimPreamble)); err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for {
		if idx := bytes.Index(l.pending, []byte(transferClaimOK)); idx >= 0 {
			l.pending = append(l.pending[:idx:idx], l.pending[idx+len(transferClaimOK):]...)
			return nil
		}
		if bytes.Contains(l.pending, []byte(transferClaimBusy)) {
			return ErrTransferPortBusy
		}
		if err := l.fill(deadline); err != nil {
			if errors.Is(err, errZTimeout) {
				return errors.New("session worker did not answer the transfer claim; restart it with sio stop and sio open")
			}
			return err
		}
	}
}

// ---- progress ----

type zProgressLine struct {
	s        *zSession
	verb     string
	name     string
	total    int64
	start    time.Time
	last     time.Time
	lastDone int64
	shown    bool
}

func (s *zSession) startProgress(verb, name string, total int64, done int64) {
	now := time.Now()
	s.progress = &zProgressLine{s: s, verb: verb, name: name, total: total, start: now, last: now, lastDone: done}
	s.live.Done, s.live.ResumeFrom = done, done
	s.report(ZPhaseData)
}

func (s *zSession) updateProgress(done int64) {
	p := s.progress
	if p == nil {
		return
	}
	s.live.Done = done
	s.report("")
	if s.opts.Progress != nil {
		return
	}
	now := time.Now()
	if now.Sub(p.last) < time.Second {
		return
	}
	rate := kbRate(done-p.lastDone, now.Sub(p.last))
	p.last = now
	p.lastDone = done
	line := fmt.Sprintf("zmodem: %s %s %d", p.verb, p.name, done)
	if p.total > 0 {
		eta := "?"
		if rate > 0 {
			left := float64(p.total-done) / 1024 / rate
			eta = time.Duration(left * float64(time.Second)).Round(time.Second).String()
		}
		line += fmt.Sprintf("/%d (%d%%) %.1f KB/s eta %s", p.total, done*100/p.total, rate, eta)
	} else {
		line += fmt.Sprintf(" bytes %.1f KB/s", rate)
	}
	if s.opts.ProgressInline {
		_, _ = fmt.Fprint(s.out, "\r\x1b[2K"+line)
		p.shown = true
	} else {
		_, _ = fmt.Fprintln(s.out, line)
	}
}

func (s *zSession) endProgress() {
	if s.progress != nil && s.progress.shown {
		_, _ = fmt.Fprint(s.out, "\r\x1b[2K")
		s.progress.shown = false
	}
}

func kbRate(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) / 1024 / d.Seconds()
}

func (s *zSession) summarize(verb string, f ZFileResult) {
	switch f.Status {
	case OutcomeSkipped:
		s.logf("skipped %s (%s)", f.Name, f.Reason)
		return
	case OutcomeRefused:
		s.logf("refused %s: %s", f.Name, f.Reason)
		return
	case OutcomeFailed:
		s.logf("failed %s: %s", f.Name, f.Reason)
		return
	}
	target := ""
	if verb == "received" && f.Path != "" && f.Path != f.Name {
		target = " -> " + f.Path
	}
	s.logf("%s %s%s %d bytes in %s (%.1f KB/s) resumed-from=%d zrpos=%d crc-errors=%d",
		verb, f.Name, target, f.Size, f.Elapsed.Round(time.Millisecond), kbRate(f.Bytes, f.Elapsed),
		f.ResumeFrom, f.Retransmits, f.CRCErrors)
	if verb == "sent" {
		s.logf("link window=%d peer-buffer=%d checkpoint=%d subpacket=%d..%d window-blocks=%d crc=%s",
			s.window, s.res.PeerBuffer, s.window/4, f.MinBlock, f.MaxBlock, f.WindowBlocks, crcName(s.crc32))
	}
}

func crcName(crc32 bool) string {
	if crc32 {
		return "crc32"
	}
	return "crc16"
}

// ---- sender ----

func (s *zSession) sendBin(h zHeader) error {
	return s.l.write(encodeBinHeader(h, s.crc32, &s.esc))
}

func (s *zSession) sendData(data []byte, end byte) error {
	return s.l.write(encodeSubpacket(nil, data, end, s.crc32, &s.esc))
}

func (s *zSession) send() error {
	s.esc.ctl = s.opts.EscapeCtl
	s.l.echo = s.text
	s.l.flow = true
	s.live.Send = true
	s.report(ZPhaseHandshake)
	if err := s.senderHandshake(); err != nil {
		return err
	}
	s.live.Window, s.live.CRC32 = s.window, s.crc32
	files := s.opts.Files
	if len(files) == 0 && s.opts.PickFiles != nil {
		s.report(ZPhasePick)
		picked, err := s.pickWatched(s.opts.PickFiles)
		if err != nil {
			return err
		}
		files = picked
		// The receiver kept repeating ZRINIT while the person chose.
		if err := s.dropQueued(zRINIT); err != nil {
			return err
		}
	}
	if len(files) == 0 {
		return errors.New("no file to send")
	}
	s.l.echo = nil
	s.live.Files = len(files)
	var total int64
	sizes := make([]int64, len(files))
	for i, f := range files {
		sizes[i] = sendFileSize(f)
		total += sizes[i]
	}
	for i, f := range files {
		r, err := s.sendFile(f, len(files)-i, total)
		total -= sizes[i]
		s.res.Files = append(s.res.Files, r)
		s.summarize("sent", r)
		if err != nil {
			return err
		}
	}
	s.report(ZPhaseFinish)
	return s.senderFinish()
}

func sendFileSize(f ZSendFile) int64 {
	if f.Path == "" {
		return int64(len(f.Data))
	}
	if info, err := os.Stat(f.Path); err == nil {
		return info.Size()
	}
	return 0
}

// senderHandshake sends "rz\r" and ZRQINIT, then waits for ZRINIT.
func (s *zSession) senderHandshake() error {
	if err := s.l.write([]byte("rz\r")); err != nil {
		return err
	}
	if err := s.l.sendHex(zPosHeader(zRQINIT, 0)); err != nil {
		return err
	}
	deadline := time.Now().Add(s.opts.HandshakeWait)
	resend := time.Now().Add(zRQInitResend)
	for {
		wait := minDuration(time.Until(resend), time.Until(deadline))
		h, err := s.l.readHeader(wait)
		if errors.Is(err, errZTimeout) || errors.Is(err, errZBadHeader) {
			if !time.Now().Before(deadline) {
				return fmt.Errorf("no ZRINIT from the receiver within %s", s.opts.HandshakeWait)
			}
			if !time.Now().Before(resend) {
				if err := s.l.sendHex(zPosHeader(zRQINIT, 0)); err != nil {
					return err
				}
				resend = time.Now().Add(zRQInitResend)
			}
			continue
		}
		if err != nil {
			return err
		}
		switch h.Type {
		case zRINIT:
			s.useReceiverInit(h)
			if s.opts.EscapeCtl {
				return s.sendSInit()
			}
			return nil
		case zCHALLENGE:
			if err := s.l.sendHex(zPosHeader(zACK, h.pos())); err != nil {
				return err
			}
		case zABORT, zFERR, zFIN:
			return s.peerGaveUp(h)
		}
	}
}

// peerGaveUp ends the session on ZABORT, ZFERR or ZFIN from the peer. It
// has stopped listening, so nothing more is written.
func (s *zSession) peerGaveUp(h zHeader) error {
	if h.Type == zABORT && s.opts.Receive {
		// lrzsz and the device answer a sender's ZABORT with ZFIN.
		_ = s.l.sendHex(zPosHeader(zFIN, 0))
	}
	s.l.gone = true
	if h.Type == zABORT {
		return errZPeerAbort
	}
	return fmt.Errorf("peer ended the session with %s", zFrameName(h.Type))
}

// dropQueued discards headers of type t (and the subpackets after ZFILE)
// that have already arrived: repeats the peer sent while this side was
// busy. They predate our answer, so answering each would start a storm.
func (s *zSession) dropQueued(t byte) error {
	buf := make([]byte, zRecvSubpacketMax)
	if err := s.l.poll(); err != nil {
		return err
	}
	if s.l.peerCancelled() {
		return errZPeerCancel
	}
	for {
		h, ok, err := s.l.pollHeader()
		if err != nil || !ok {
			return err
		}
		if h.Type != t {
			s.l.unread(h)
			return nil
		}
		if t == zFILE {
			_, _, err := s.l.readSubpacket(buf, zRecvSubpacketMax, h.Format == zBIN32, s.opts.DataWait)
			if errors.Is(err, errZPeerCancel) || errors.Is(err, errZLocalCancel) || errors.Is(err, errZLinkClosed) {
				return err
			}
		}
	}
}

// whilePeerWaits runs fn, which waits for a person (a file dialog), and
// meanwhile watches the link. If the peer gives up (cancel sequence) or
// Cancel fires first, ClosePicker is asked to end the dialog and the
// session ends without writing anything into what is now the device
// shell.
func (s *zSession) whilePeerWaits(fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	for {
		select {
		case err := <-done:
			if s.l.peerCancelled() {
				return errZPeerCancel
			}
			return err
		default:
		}
		err := s.l.fill(time.Now().Add(50 * time.Millisecond))
		gone := s.l.peerCancelled()
		if gone || errors.Is(err, errZLocalCancel) || errors.Is(err, errZLinkClosed) {
			if s.opts.ClosePicker != nil {
				s.opts.ClosePicker()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			switch {
			case gone:
				return errZPeerCancel
			case err != nil:
				return err
			}
		}
		if err != nil && !errors.Is(err, errZTimeout) {
			return err
		}
	}
}

func (s *zSession) pickWatched(pick func() ([]ZSendFile, error)) ([]ZSendFile, error) {
	var files []ZSendFile
	err := s.whilePeerWaits(func() error {
		var err error
		files, err = pick()
		return err
	})
	return files, err
}

func (s *zSession) destinationWatched(info ZFileInfo) (ZDestination, error) {
	if s.opts.Destination == nil {
		return s.destination(info)
	}
	var dest ZDestination
	s.report(ZPhasePick)
	err := s.whilePeerWaits(func() error {
		var err error
		dest, err = s.destination(info)
		return err
	})
	s.report(ZPhaseOffer)
	if errors.Is(err, errZPeerCancel) || errors.Is(err, errZLocalCancel) || errors.Is(err, errZLinkClosed) {
		return ZDestination{}, err
	}
	return dest, err
}

// answerWait is how long to wait for a peer that may first compute a CRC
// over size bytes: the handshake wait (at least the 120 s default) plus
// time for a slow card.
func (s *zSession) answerWait(size int64) time.Duration {
	wait := max(s.opts.HandshakeWait, defaultZHandshakeWait)
	if size > 0 {
		wait += time.Duration(size / zSlowCRCRate * int64(time.Second))
	}
	return wait
}

func (s *zSession) useReceiverInit(h zHeader) {
	flags := h.f0()
	s.fullDuplex = flags&zCanFDX != 0
	s.crc32 = flags&zCanFC32 != 0
	if flags&zEscCtl != 0 {
		s.esc.ctl = true
	}
	s.res.PeerBuffer = int(h.Data[0]) | int(h.Data[1])<<8
	s.window = s.opts.Window
	if s.res.PeerBuffer > 0 && s.res.PeerBuffer < s.window {
		s.window = s.res.PeerBuffer
	}
	s.maxBlock = s.opts.MaxSubpacket
	if s.res.PeerBuffer > 0 && s.maxBlock > s.res.PeerBuffer {
		s.maxBlock = max(zMinSubpacket, s.res.PeerBuffer)
	}
	s.res.Window = s.window
	s.res.CRC32 = s.crc32
	s.res.EscapeCtl = s.esc.ctl
	s.res.FullDuplex = s.fullDuplex
}

// sendSInit asks the receiver to escape control characters too.
func (s *zSession) sendSInit() error {
	for try := 0; try < 4; try++ {
		if err := s.sendBin(zFlagsHeader(zSINIT, zTEscCtl, 0, 0, 0)); err != nil {
			return err
		}
		if err := s.sendData([]byte{0}, zCRCW); err != nil {
			return err
		}
		h, err := s.l.readHeader(s.opts.DataWait)
		if errors.Is(err, errZTimeout) || errors.Is(err, errZBadHeader) {
			continue
		}
		if err != nil {
			return err
		}
		if h.Type == zACK {
			return nil
		}
	}
	return errors.New("receiver did not acknowledge ZSINIT")
}

func zFileInfoBlock(name string, size int64, mtime time.Time, filesLeft int, bytesLeft int64) []byte {
	secs := int64(0)
	if !mtime.IsZero() {
		secs = mtime.Unix()
	}
	return []byte(fmt.Sprintf("%s\x00%d %o %o 0 %d %d\x00", name, size, secs, 0o100644, filesLeft, bytesLeft))
}

type zSource struct {
	r     io.ReaderAt
	size  int64
	mtime time.Time
	close func()
}

func openZSource(f ZSendFile) (zSource, error) {
	if f.Path == "" {
		return zSource{r: bytes.NewReader(f.Data), size: int64(len(f.Data)), mtime: f.ModTime, close: func() {}}, nil
	}
	file, err := os.Open(f.Path)
	if err != nil {
		return zSource{}, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return zSource{}, err
	}
	if info.IsDir() {
		_ = file.Close()
		return zSource{}, errors.New("is a directory")
	}
	mtime := f.ModTime
	if mtime.IsZero() {
		mtime = info.ModTime()
	}
	return zSource{r: file, size: info.Size(), mtime: mtime, close: func() { _ = file.Close() }}, nil
}

func sendName(f ZSendFile) string {
	name := f.Name
	if name == "" {
		name = filepath.Base(f.Path)
	}
	return filepath.Base(strings.ReplaceAll(name, "\\", "/"))
}

// prefixCRC is the CRC32 of the first n bytes; n of 0 means all of them.
func prefixCRC(r io.ReaderAt, size int64, n int64) uint32 {
	if n <= 0 || n > size {
		n = size
	}
	h := crc32.NewIEEE()
	_, _ = io.Copy(h, io.NewSectionReader(r, 0, n))
	return h.Sum32()
}

// sendFile offers one file and sends it from the position the receiver
// asks for. A per-file failure is reported in the result; the returned
// error ends the session.
func (s *zSession) sendFile(f ZSendFile, filesLeft int, bytesLeft int64) (ZFileResult, error) {
	name := sendName(f)
	res := ZFileResult{Name: name, Path: f.Path, MinBlock: s.maxBlock, MaxBlock: s.maxBlock}
	s.reportFile(ZPhaseOffer, name, sendFileSize(f), s.live.Files-filesLeft+1, &res)
	src, err := openZSource(f)
	if err != nil {
		res.Status, res.Reason = OutcomeFailed, err.Error()
		return res, nil
	}
	defer src.close()
	res.Size = src.size
	if s.opts.sourceHook != nil {
		s.opts.sourceHook(name, &src)
	}

	conv := zCRESUM
	if s.opts.NoResume {
		conv = zCBIN
	}
	fileHeader := zFlagsHeader(zFILE, conv, s.opts.Conflict.zmanagement(), 0, 0)
	info := zFileInfoBlock(name, src.size, src.mtime, filesLeft, bytesLeft)
	if s.opts.noSize {
		info = append([]byte(name), 0)
	}
	offer := func() error {
		if err := s.sendBin(fileHeader); err != nil {
			return err
		}
		return s.sendData(info, zCRCW)
	}
	if err := offer(); err != nil {
		return res, err
	}
	// The receiver may be a person choosing where to save, or a device
	// computing the CRC of a large file it already has.
	wait := s.answerWait(src.size)
	deadline := time.Now().Add(wait)
	resend := time.Now().Add(zFileResend)
	// Once the receiver has answered with anything, ZFILE is repeated only
	// on its request (ZNAK, or a ZRINIT followed by silence); its silence
	// alone then means it is busy, for example computing a CRC.
	spoke := false
	asked := false
	crcAsked := false
	var start int64
	for waiting := true; waiting; {
		h, err := s.l.readHeader(minDuration(time.Until(resend), time.Until(deadline)))
		if errors.Is(err, errZTimeout) || errors.Is(err, errZBadHeader) {
			if !time.Now().Before(deadline) {
				return res, fmt.Errorf("receiver did not answer ZFILE for %s within %s", name, wait)
			}
			if !time.Now().Before(resend) {
				if !spoke || asked {
					if err := offer(); err != nil {
						return res, err
					}
				}
				resend = time.Now().Add(zFileResend)
			}
			// A ZRINIT asks for one more offer only if silence follows it.
			asked = false
			continue
		}
		if err != nil {
			return res, err
		}
		if h.Type != zRINIT {
			spoke = true
			asked = false
		}
		switch h.Type {
		case zRPOS:
			start = int64(h.pos())
			waiting = false
		case zSKIP:
			// Only a receiver that asked for our CRC compared contents; a
			// plain ZSKIP keeps its own file (the device refuses an
			// existing name unless rz -o or -n).
			if crcAsked {
				res.Status, res.Reason = OutcomeSkipped, "identical on the receiver"
			} else {
				res.Status, res.Reason = OutcomeRefused, "receiver refused it (exists)"
			}
			return res, nil
		case zCRC:
			crcAsked = true
			s.report(ZPhaseCheck)
			if s.opts.ignoreZCRC {
				continue
			}
			crc := prefixCRC(src.r, src.size, int64(h.pos()))
			if err := s.sendBin(zPosHeader(zCRC, crc)); err != nil {
				return res, err
			}
		case zRINIT:
			// Usually a second answer to ZRQINIT, already on its way when
			// ZFILE went out. Offer again only if nothing else follows,
			// as lrzsz does; an extra ZFILE costs the receiver a resync.
			resend = minTime(resend, time.Now().Add(zStaleRInitWait))
			asked = true
		case zNAK:
			if err := offer(); err != nil {
				return res, err
			}
			resend = time.Now().Add(zFileResend)
		case zCHALLENGE:
			if err := s.l.sendHex(zPosHeader(zACK, h.pos())); err != nil {
				return res, err
			}
		case zFERR, zABORT, zFIN:
			return res, s.peerGaveUp(h)
		}
	}
	if start > src.size {
		start = src.size
	}
	res.ResumeFrom = start
	began := time.Now()
	s.startProgress("send", name, src.size, start)
	skipped, err := s.streamFile(src, start, &res)
	res.Elapsed = time.Since(began)
	var rerr *zReadError
	if errors.As(err, &rerr) {
		// The file is cut short at the last good offset; the receiver
		// sees a short file and the batch goes on.
		res.Status, res.Reason = OutcomeFailed, rerr.Error()
		return res, nil
	}
	if err != nil {
		res.Status, res.Reason = OutcomeFailed, err.Error()
		return res, err
	}
	if skipped {
		res.Status, res.Reason = OutcomeRefused, "receiver skipped it during data"
		return res, nil
	}
	res.Bytes = src.size - start
	res.Status = OutcomeOK
	if start > 0 {
		res.Status = OutcomeResumed
	}
	return res, nil
}

// zReadError is a local read failure on the file being sent.
type zReadError struct{ err error }

func (e *zReadError) Error() string { return "read error: " + e.err.Error() }

// cutFile ends the file being sent at the last good offset after a read
// error: the open frame is closed and ZEOF goes out there, so the
// receiver sees a short file. The batch then goes on.
func (s *zSession) cutFile(st *zStream, src zSource, res *ZFileResult, cause error) error {
	if st.frameOpen {
		if err := s.sendData(nil, zCRCE); err != nil {
			return err
		}
		st.frameOpen = false
	}
	short := src
	short.size = st.pos
	if err := s.sendEOF(short, st, res); err != nil {
		return err
	}
	return &zReadError{err: cause}
}

// zStream is the sender state for one file.
type zStream struct {
	pos      int64
	acked    int64
	block    int
	goodRun  int
	sinceChk int
	newFrame bool
	// frameAt is where the open frame's ZDATA header pointed, and
	// frameData when its first subpacket went out (zero before that).
	// echoed is set once a ZRPOS for frameAt was taken for a repeat.
	frameAt   int64
	frameData time.Time
	echoed    bool
	lastRpos  int64
	sameRpos  int
	eofSent   bool
	done      bool
	skipped   bool
	frameOpen bool
}

// streamFile sends data subpackets from start. In full duplex it streams
// ZCRCG, puts a ZCRCQ checkpoint every window/4 bytes and stops only when
// window bytes are unacknowledged. A ZRPOS rewinds to the asked position
// and halves the subpacket length.
func (s *zSession) streamFile(src zSource, start int64, res *ZFileResult) (bool, error) {
	// The ZRPOS that answered ZFILE is the first one asking for start.
	st := &zStream{pos: start, acked: start, block: s.maxBlock, newFrame: true, lastRpos: start}
	buf := make([]byte, s.maxBlock)
	for !st.done {
		// Look at the reverse channel without waiting.
		for {
			h, ok, err := s.l.pollHeader()
			if err != nil {
				return false, err
			}
			if !ok {
				break
			}
			if err := s.handleReverse(h, st, src, res); err != nil {
				return false, err
			}
		}
		if st.done {
			break
		}
		if s.l.xoff {
			if err := s.waitXon(st, src, res); err != nil {
				return false, err
			}
			continue
		}
		if st.pos >= src.size {
			if err := s.sendEOF(src, st, res); err != nil {
				return false, err
			}
			continue
		}
		if s.fullDuplex && st.pos-st.acked >= int64(s.window) {
			res.WindowBlocks++
			if err := s.waitAck(st, src, res); err != nil {
				return false, err
			}
			continue
		}
		if st.newFrame {
			if err := s.sendBin(zPosHeader(zDATA, uint32(st.pos))); err != nil {
				return false, err
			}
			st.newFrame = false
			st.frameOpen = true
			st.frameAt = st.pos
			st.frameData = time.Time{}
			st.echoed = false
			st.sinceChk = 0
		}
		n, err := src.r.ReadAt(buf[:min(int64(st.block), src.size-st.pos)], st.pos)
		if (err != nil && !errors.Is(err, io.EOF)) || n == 0 {
			if err == nil || errors.Is(err, io.EOF) {
				err = fmt.Errorf("file shrank at %d", st.pos)
			}
			return false, s.cutFile(st, src, res, err)
		}
		end := zCRCG
		st.sinceChk += n
		switch {
		case !s.fullDuplex:
			end = zCRCW
		case st.pos+int64(n) >= src.size:
			end = zCRCE
		case st.sinceChk >= s.window/4:
			end = zCRCQ
			st.sinceChk = 0
		}
		if err := s.sendData(buf[:n], end); err != nil {
			return false, err
		}
		if st.frameData.IsZero() {
			st.frameData = time.Now()
		}
		st.pos += int64(n)
		if s.opts.trace != nil {
			s.opts.trace(st.pos, st.acked)
		}
		s.updateProgress(st.pos)
		st.goodRun++
		if st.goodRun >= zGrowAfter && st.block < s.maxBlock {
			st.block = min(st.block*2, s.maxBlock)
			st.goodRun = 0
			res.MaxBlock = max(res.MaxBlock, st.block)
		}
		if end == zCRCW || end == zCRCE {
			st.newFrame = true
			st.frameOpen = false
		}
		if end == zCRCW {
			if err := s.waitAck(st, src, res); err != nil {
				return false, err
			}
		}
	}
	return st.skipped, nil
}

// handleReverse applies one header from the receiver during data.
func (s *zSession) handleReverse(h zHeader, st *zStream, src zSource, res *ZFileResult) error {
	switch h.Type {
	case zACK:
		if p := int64(h.pos()); p > st.acked && p <= st.pos {
			st.acked = p
		}
	case zRPOS:
		p := int64(h.pos())
		if p > src.size {
			p = src.size
		}
		// Every ZRPOS counts against the retry limit, whatever it leads
		// to: a receiver that keeps asking for one offset ends the file.
		if p == st.lastRpos {
			st.sameRpos++
			if st.sameRpos > zMaxRetries {
				return fmt.Errorf("receiver asked for offset %d %d times", p, st.sameRpos+1)
			}
		} else {
			st.lastRpos = p
			st.sameRpos = 0
		}
		if s.rposEcho(st, p) {
			// The open frame starts at p and the receiver sent this
			// before it could have read any of the frame: a second answer
			// to ZFILE, or a ZRPOS repeated in a burst. Restarting here
			// would cut the frame short and resend what it is reading.
			// waitAck rewinds after all if no ZACK follows.
			st.echoed = true
			return nil
		}
		if err := s.rewind(st, p, res); err != nil {
			return err
		}
		// The receiver has everything up to p. A closed frame gets a fresh
		// ZDATA anyway; a sent ZEOF is repeated.
		st.acked = max(st.acked, p)
		st.eofSent = false
	case zSKIP:
		st.done, st.skipped = true, true
	case zRINIT:
		if st.eofSent {
			st.done = true
		}
	case zCRC:
		return s.sendBin(zPosHeader(zCRC, prefixCRC(src.r, src.size, int64(h.pos()))))
	case zCHALLENGE:
		return s.l.sendHex(zPosHeader(zACK, h.pos()))
	case zFIN, zABORT, zFERR:
		return s.peerGaveUp(h)
	}
	return nil
}

// rposEcho reports whether a ZRPOS for p repeats a request the open frame
// already answers: the frame starts at p, and the ZRPOS arrived before the
// frame's first subpacket went out or so soon after it that the receiver
// cannot have read that subpacket and complained about it. Any other
// ZRPOS for p means the frame went wrong, even in its first subpacket.
func (s *zSession) rposEcho(st *zStream, p int64) bool {
	if !st.frameOpen || p != st.frameAt {
		return false
	}
	return st.frameData.IsZero() || s.l.rxAt.Before(st.frameData.Add(zRposEcho))
}

// rewind restarts the data at p. Only a rewind that throws away sent data
// is a line error and halves the subpacket; one at the current position,
// such as a repeated answer to ZFILE, just restates where the next frame
// starts. An open frame ends first with an empty ZCRCE, since a receiver
// never sees a new ZDATA header inside a frame, and a fresh ZDATA follows,
// as the device sender does.
func (s *zSession) rewind(st *zStream, p int64, res *ZFileResult) error {
	if p < st.pos {
		res.Retransmits++
		s.report("")
		st.block = max(zMinSubpacket, st.block/2)
		res.MinBlock = min(res.MinBlock, st.block)
		st.goodRun = 0
	}
	if st.frameOpen {
		if err := s.sendData(nil, zCRCE); err != nil {
			return err
		}
		st.newFrame = true
		st.frameOpen = false
	}
	if p < st.pos {
		st.pos = p
		st.newFrame = true
	}
	return nil
}

// waitXon holds the data after the receiver sent XOFF, until its XON or
// until zXoffWait passes without one. Headers that come meanwhile are
// handled.
func (s *zSession) waitXon(st *zStream, src zSource, res *ZFileResult) error {
	wait := zXoffWait
	if s.opts.xoffWait > 0 {
		wait = s.opts.xoffWait
	}
	deadline := time.Now().Add(wait)
	for s.l.xoff && !st.done {
		h, ok, err := s.l.pollHeader()
		if err != nil {
			return err
		}
		if ok {
			if err := s.handleReverse(h, st, src, res); err != nil {
				return err
			}
			continue
		}
		if !time.Now().Before(deadline) {
			s.l.xoff = false
			break
		}
		if err := s.l.fill(deadline); err != nil && !errors.Is(err, errZTimeout) {
			return err
		}
	}
	return nil
}

// waitAck waits for the receiver to move the window or answer a ZCRCW.
// When nothing comes, an empty ZCRCQ subpacket asks for a fresh ZACK.
func (s *zSession) waitAck(st *zStream, src zSource, res *ZFileResult) error {
	acked, pos := st.acked, st.pos
	// Headers that do not move the window still count: a receiver that
	// keeps sending strays cannot hold the sender here forever.
	deadline := time.Now().Add(zAckPokeWait * (zAckPokes + 2))
	for poke := 0; ; poke++ {
		if !time.Now().Before(deadline) {
			return errors.New("receiver stopped acknowledging data")
		}
		h, err := s.l.readHeader(minDuration(zAckPokeWait, time.Until(deadline)))
		if errors.Is(err, errZBadHeader) {
			continue
		}
		if errors.Is(err, errZTimeout) {
			if st.frameOpen && st.echoed {
				// A ZRPOS for the frame start was taken for a repeat and
				// nothing has moved since: it was about this frame after
				// all, and the receiver is still hunting for a header.
				return s.rewind(st, st.frameAt, res)
			}
			if poke >= zAckPokes {
				return errors.New("receiver stopped acknowledging data")
			}
			if st.frameOpen {
				if err := s.sendData(nil, zCRCQ); err != nil {
					return err
				}
			} else {
				// The frame is closed: restate the position.
				st.newFrame = true
				return nil
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := s.handleReverse(h, st, src, res); err != nil {
			return err
		}
		if st.done || st.acked != acked || st.pos != pos || (h.Type == zACK && !s.fullDuplex) {
			return nil
		}
	}
}

// sendEOF sends ZEOF and waits for ZRINIT, which ends the file.
func (s *zSession) sendEOF(src zSource, st *zStream, res *ZFileResult) error {
	for try := 0; try < zMaxRetries; try++ {
		if err := s.sendBin(zPosHeader(zEOF, uint32(src.size))); err != nil {
			return err
		}
		st.eofSent = true
		for {
			h, err := s.l.readHeader(s.opts.DataWait)
			if errors.Is(err, errZTimeout) || errors.Is(err, errZBadHeader) {
				break
			}
			if err != nil {
				return err
			}
			if err := s.handleReverse(h, st, src, res); err != nil {
				return err
			}
			if st.done || !st.eofSent {
				return nil
			}
		}
	}
	return errors.New("receiver did not acknowledge ZEOF")
}

// senderFinish sends ZFIN, waits for the answering ZFIN and sends "OO".
func (s *zSession) senderFinish() error {
	for try := 0; try < 5; try++ {
		if err := s.l.sendHex(zPosHeader(zFIN, 0)); err != nil {
			return err
		}
		for {
			h, err := s.l.readHeader(3 * time.Second)
			if errors.Is(err, errZTimeout) {
				break
			}
			if errors.Is(err, errZBadHeader) {
				continue
			}
			if err != nil {
				return err
			}
			if h.Type == zFIN {
				return s.l.write([]byte("OO"))
			}
		}
	}
	// Every file went through; a lost ZFIN is not worth a failure.
	return nil
}

// ---- receiver ----

func (s *zSession) rinit() error {
	flags := zCanFDX | zCanOVIO | zCanFC32
	if s.opts.halfDuplex {
		flags &^= zCanFDX
	}
	if s.opts.EscapeCtl {
		flags |= zEscCtl
	}
	h := zFlagsHeader(zRINIT, flags, 0, 0, 0)
	buf := min(s.opts.RecvBuffer, 0xffff)
	h.Data[0], h.Data[1] = byte(buf), byte(buf>>8)
	return s.l.sendHex(h)
}

func (s *zSession) receive() error {
	s.l.ctlIgnore = s.opts.EscapeCtl
	s.esc.ctl = s.opts.EscapeCtl
	s.l.echo = s.text
	s.res.CRC32 = true
	s.res.FullDuplex = !s.opts.halfDuplex
	s.res.EscapeCtl = s.opts.EscapeCtl
	s.res.PeerBuffer = s.opts.RecvBuffer
	s.live.Window, s.live.CRC32 = s.opts.RecvBuffer, true
	s.report(ZPhaseHandshake)
	if err := s.rinit(); err != nil {
		return err
	}
	wait := s.opts.HandshakeWait
	index := 0
	buf := make([]byte, zRecvSubpacketMax)
	var lastOffer []byte
	var answered time.Time
	spoke := false
	for {
		deadline := time.Now().Add(wait)
		h, err := s.waitSender(deadline, &spoke)
		if err != nil {
			return err
		}
		switch h.Type {
		case zFIN:
			s.report(ZPhaseFinish)
			s.endSession()
			return nil
		case zFILE:
			data, _, err := s.l.readSubpacket(buf, zRecvSubpacketMax, h.Format == zBIN32, s.opts.DataWait)
			if errors.Is(err, errZPeerCancel) || errors.Is(err, errZLocalCancel) || errors.Is(err, errZLinkClosed) {
				return err
			}
			if err != nil {
				if err := s.l.sendHex(zPosHeader(zNAK, 0)); err != nil {
					return err
				}
				continue
			}
			if bytes.Equal(data, lastOffer) && time.Since(answered) < zRepeatWindow {
				// Sent before our answer reached the sender.
				continue
			}
			lastOffer = append(lastOffer[:0], data...)
			s.l.echo = nil
			info := parseZFileInfo(data)
			info.Index = index
			index++
			r, fin, err := s.receiveFile(h, info)
			answered = time.Now()
			if r.Name != "" {
				s.res.Files = append(s.res.Files, r)
				s.summarize("received", r)
			}
			if err != nil {
				return err
			}
			if fin {
				s.report(ZPhaseFinish)
				s.endSession()
				return nil
			}
			if err := s.rinit(); err != nil {
				return err
			}
			s.report(ZPhaseHandshake)
			// A new phase: the next ZFILE follows at once, unless a
			// person is choosing.
			spoke = false
			wait = s.opts.HandshakeWait
		}
	}
}

// waitSender waits for ZFILE or ZFIN and answers the other session frames.
// ZRINIT is repeated on a timer only while the peer has said nothing in
// this phase (spoke); after that only on request (ZRQINIT, a repeated
// ZEOF, a damaged header that may have been the offer).
func (s *zSession) waitSender(deadline time.Time, spoke *bool) (zHeader, error) {
	resend := time.Now().Add(zRInitResend)
	lastRInit := time.Now()
	buf := make([]byte, zRecvSubpacketMax)
	// answer repeats ZRINIT on an explicit request; damaged headers are
	// rate limited so a noisy line does not turn into a ZRINIT storm.
	answer := func(limit bool) error {
		if limit && time.Since(lastRInit) < zBadHeaderRInit {
			return nil
		}
		lastRInit = time.Now()
		resend = time.Now().Add(zRInitResend)
		return s.rinit()
	}
	for {
		h, err := s.l.readHeader(minDuration(time.Until(resend), time.Until(deadline)))
		if errors.Is(err, errZTimeout) || errors.Is(err, errZBadHeader) {
			if !time.Now().Before(deadline) {
				return h, fmt.Errorf("no file offer from the sender within %s", s.opts.HandshakeWait)
			}
			if errors.Is(err, errZBadHeader) {
				// A damaged header may have been a ZFILE: ask again, but
				// not once per damaged header.
				if err := answer(true); err != nil {
					return h, err
				}
				continue
			}
			if !time.Now().Before(resend) {
				resend = time.Now().Add(zRInitResend)
				if !*spoke {
					lastRInit = time.Now()
					if err := s.rinit(); err != nil {
						return h, err
					}
				}
			}
			continue
		}
		if err != nil {
			return h, err
		}
		*spoke = true
		switch h.Type {
		case zFILE, zFIN:
			return h, nil
		case zABORT:
			return h, s.peerGaveUp(h)
		case zRQINIT, zEOF:
			// The sender missed our ZRINIT.
			if err := answer(false); err != nil {
				return h, err
			}
		case zSINIT:
			data, _, err := s.l.readSubpacket(buf, zRecvSubpacketMax, h.Format == zBIN32, s.opts.DataWait)
			if err != nil {
				if err := s.l.sendHex(zPosHeader(zNAK, 0)); err != nil {
					return h, err
				}
				continue
			}
			if h.f0()&zTEscCtl != 0 {
				s.esc.ctl = true
				s.res.EscapeCtl = true
			}
			s.attn = append([]byte(nil), bytes.TrimRight(data, "\x00")...)
			if err := s.l.sendHex(zPosHeader(zACK, 1)); err != nil {
				return h, err
			}
		case zFREECNT:
			if err := s.l.sendHex(zPosHeader(zACK, zFreeSpace(s.destDirHint()))); err != nil {
				return h, err
			}
		case zCOMMAND:
			_, _, _ = s.l.readSubpacket(buf, zRecvSubpacketMax, h.Format == zBIN32, s.opts.DataWait)
			s.logf("refused a remote command (ZCOMMAND)")
			if err := s.l.sendHex(zPosHeader(zCOMPL, 1)); err != nil {
				return h, err
			}
		case zCHALLENGE:
			if err := s.l.sendHex(zPosHeader(zACK, h.pos())); err != nil {
				return h, err
			}
		}
	}
}

func (s *zSession) destDirHint() string {
	if s.opts.DestDir != "" {
		return s.opts.DestDir
	}
	if s.opts.DestPath != "" {
		return filepath.Dir(s.opts.DestPath)
	}
	return "."
}

// endSession answers the sender's ZFIN and lets its "OO" pass.
func (s *zSession) endSession() {
	_ = s.l.sendHex(zPosHeader(zFIN, 0))
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if bytes.Contains(s.l.pending, []byte("OO")) {
			idx := bytes.Index(s.l.pending, []byte("OO"))
			s.l.pending = s.l.pending[idx+2:]
			return
		}
		if err := s.l.fill(deadline); err != nil {
			return
		}
	}
}

// parseZFileInfo reads "name\0size mtime mode serial filesleft bytesleft".
// Every field after the name may be missing.
func parseZFileInfo(data []byte) ZFileInfo {
	info := ZFileInfo{Size: -1}
	nameEnd := bytes.IndexByte(data, 0)
	if nameEnd < 0 {
		nameEnd = len(data)
	}
	info.RawName = string(data[:nameEnd])
	info.Name = SanitizeTransferName(info.RawName)
	rest := []byte{}
	if nameEnd < len(data) {
		rest = data[nameEnd+1:]
	}
	if end := bytes.IndexByte(rest, 0); end >= 0 {
		rest = rest[:end]
	}
	fields := strings.Fields(string(rest))
	if len(fields) > 0 {
		if v, err := strconv.ParseInt(fields[0], 10, 64); err == nil && v >= 0 {
			info.Size = v
		}
	}
	if len(fields) > 1 {
		if v, err := strconv.ParseInt(fields[1], 8, 64); err == nil && v > 0 {
			info.ModTime = time.Unix(v, 0)
		}
	}
	if len(fields) > 2 {
		if v, err := strconv.ParseUint(fields[2], 8, 32); err == nil {
			info.Mode = uint32(v)
		}
	}
	if len(fields) > 4 {
		if v, err := strconv.Atoi(fields[4]); err == nil {
			info.FilesLeft = v
		}
	}
	if len(fields) > 5 {
		if v, err := strconv.ParseInt(fields[5], 10, 64); err == nil {
			info.BytesLeft = v
		}
	}
	return info
}

var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// SanitizeTransferName keeps the last path element of a sender's file name
// and replaces characters Windows does not allow. Drive letters, ".." and
// absolute paths cannot survive it. It returns "" when nothing is left.
func SanitizeTransferName(raw string) string {
	name := strings.ReplaceAll(raw, "\\", "/")
	if idx := strings.LastIndexByte(name, '/'); idx >= 0 {
		name = name[idx+1:]
	}
	if idx := strings.LastIndexByte(name, ':'); idx >= 0 {
		name = name[idx+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			b.WriteByte('_')
		case strings.ContainsRune(`<>:"|?*`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	name = strings.TrimRight(strings.TrimSpace(b.String()), ". ")
	if name == "" || name == "." || name == ".." {
		return ""
	}
	base := strings.ToUpper(name)
	if idx := strings.IndexByte(base, '.'); idx >= 0 {
		base = base[:idx]
	}
	if windowsReservedNames[base] {
		name = "_" + name
	}
	return name
}

type zAction int

const (
	zActWrite zAction = iota
	zActSkip
	zActAppend
)

// zPrefixCRC asks the sender for the CRC32 of its first n bytes. ok is
// false when the sender does not answer ZCRC.
// The request is asked up to zCRCTries times: on silence, and when the
// answer arrives damaged (a bad header). A slow card on the sender gets
// answerWait in total, split over the tries.
func (s *zSession) zPrefixCRC(n int64) (uint32, bool, error) {
	buf := make([]byte, zRecvSubpacketMax)
	wait := s.opts.crcWait
	if wait <= 0 {
		wait = s.answerWait(n)
	}
	per := wait / zCRCTries
	s.report(ZPhaseCheck)
	for try := 0; try < zCRCTries; try++ {
		if err := s.l.sendHex(zPosHeader(zCRC, uint32(n))); err != nil {
			return 0, false, err
		}
		deadline := time.Now().Add(per)
	answer:
		for {
			h, err := s.l.readHeader(time.Until(deadline))
			if errors.Is(err, errZTimeout) {
				break answer
			}
			if errors.Is(err, errZBadHeader) {
				// Possibly the answer itself; ask again at once.
				break answer
			}
			if err != nil {
				return 0, false, err
			}
			switch h.Type {
			case zCRC:
				return h.pos(), true, nil
			case zNAK:
				return 0, false, nil
			case zFILE:
				// The sender repeated its offer before seeing ZCRC.
				_, _, _ = s.l.readSubpacket(buf, zRecvSubpacketMax, h.Format == zBIN32, s.opts.DataWait)
			case zABORT, zFIN:
				return 0, false, s.peerGaveUp(h)
			}
		}
	}
	return 0, false, nil
}

func localPrefixCRC(path string, n int64) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := crc32.NewIEEE()
	if _, err := io.CopyN(h, f, n); err != nil {
		return 0, err
	}
	return h.Sum32(), nil
}

func fileExists(path string) (os.FileInfo, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return info, err == nil
	}
	return info, true
}

// renamedPath returns name_1.ext, name_2.ext, ... for the first one free.
func renamedPath(path string) string {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s_%d%s", stem, i, ext)
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
}

func (s *zSession) destination(info ZFileInfo) (ZDestination, error) {
	if s.opts.Destination != nil {
		return s.opts.Destination(info)
	}
	if info.Index == 0 && s.opts.DestPath != "" {
		return ZDestination{Path: s.opts.DestPath}, nil
	}
	dir := s.opts.DestDir
	if dir == "" {
		dir = "."
	}
	return ZDestination{Path: filepath.Join(dir, info.Name)}, nil
}

// decide picks what to do with an existing target. An explicit local
// policy wins; otherwise the sender's ZF1 management option is obeyed, and
// without one the default skip-identical-else-rename applies. ZCRC is sent
// only when the offer carries ZCRESUM or ZMCRC: without either the sender
// may not answer it, so an existing file is renamed rather than compared.
func (s *zSession) decide(h zHeader, info ZFileInfo, target string, policy ConflictPolicy, explicit bool) (zAction, string, string, error) {
	existing, exists := fileExists(target)
	if exists && existing.IsDir() {
		return zActSkip, target, "target is a directory", nil
	}
	management := h.f1() & zMMask
	if explicit {
		management = 0
	} else if h.f1()&zMSkNoLoc != 0 && !exists {
		return zActSkip, target, "not present here", nil
	}
	if !exists {
		return zActWrite, target, "", nil
	}
	crcOK := h.f0() == zCRESUM || management == zMCRC || h.f1()&zMMask == zMCRC
	identical := func() (bool, error) {
		if !crcOK || info.Size < 0 || existing.Size() != info.Size {
			return false, nil
		}
		remote, ok, err := s.zPrefixCRC(info.Size)
		if err != nil || !ok {
			return false, err
		}
		local, err := localPrefixCRC(target, info.Size)
		if err != nil {
			return false, nil
		}
		return local == remote, nil
	}
	newer := info.ModTime.IsZero() || info.ModTime.After(existing.ModTime())
	switch management {
	case zMNEWL:
		if newer || info.Size > existing.Size() {
			return zActWrite, target, "", nil
		}
		return zActSkip, target, "not newer or longer", nil
	case zMAPND:
		return zActAppend, target, "", nil
	case zMCLOB:
		return zActWrite, target, "", nil
	case zMNEW:
		if newer {
			return zActWrite, target, "", nil
		}
		return zActSkip, target, "not newer", nil
	case zMDIFF:
		if info.Size != existing.Size() || (!info.ModTime.IsZero() && !info.ModTime.Equal(existing.ModTime())) {
			return zActWrite, target, "", nil
		}
		return zActSkip, target, "same date and length", nil
	case zMPROT:
		return zActSkip, target, "exists", nil
	case zMCHNG:
		return zActWrite, renamedPath(target), "", nil
	}
	// ZMCRC, no option, or an explicit policy.
	switch policy {
	case ConflictOverwrite:
		return zActWrite, target, "", nil
	case ConflictSkip:
		return zActSkip, target, "exists", nil
	case ConflictRename:
		return zActWrite, renamedPath(target), "", nil
	}
	same, err := identical()
	if err != nil {
		return 0, "", "", err
	}
	if same {
		return zActSkip, target, "identical", nil
	}
	return zActWrite, renamedPath(target), "", nil
}

// receiveFile handles one ZFILE. A per-file failure is reported in the
// result and skipped; the error ends the session. fin is set when the
// sender ended the session in the middle of the file.
func (s *zSession) receiveFile(h zHeader, info ZFileInfo) (ZFileResult, bool, error) {
	res := ZFileResult{Name: info.Name, Size: info.Size}
	if res.Name == "" {
		res.Name = info.RawName
	}
	if info.FilesLeft > 0 {
		s.live.Files = info.Index + info.FilesLeft
	}
	s.reportFile(ZPhaseOffer, res.Name, info.Size, info.Index+1, &res)
	skip := func(status, reason string) (ZFileResult, bool, error) {
		res.Status, res.Reason = status, reason
		if err := s.dropQueued(zFILE); err != nil {
			return res, false, err
		}
		return res, false, s.l.sendHex(zPosHeader(zSKIP, 0))
	}
	if info.Name == "" {
		return skip(OutcomeFailed, "invalid file name")
	}
	dest, err := s.destinationWatched(info)
	if err != nil {
		if errors.Is(err, ErrTransferCancelled) || errors.Is(err, errZPeerCancel) || errors.Is(err, errZLocalCancel) || errors.Is(err, errZLinkClosed) {
			return ZFileResult{}, false, err
		}
		return skip(OutcomeFailed, err.Error())
	}
	if dest.Path == "" {
		return skip(OutcomeRefused, "no destination chosen")
	}
	policy := dest.Policy
	if policy == "" {
		policy = s.opts.Conflict
	}
	action, target, reason, err := s.decide(h, info, dest.Path, policy, policy != "")
	if err != nil {
		return res, false, err
	}
	res.Path = target
	if action == zActSkip {
		// Only an identical target is a clean skip; any other kept file
		// means this one was refused.
		if reason == "identical" {
			return skip(OutcomeSkipped, reason)
		}
		return skip(OutcomeRefused, reason)
	}
	if dir := filepath.Dir(target); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return skip(OutcomeFailed, err.Error())
		}
	}

	// Resume only when the sender asked for it with ZCRESUM, and only
	// after its CRC of the prefix matches. No answer means start over.
	part := target + ".zpart"
	var start int64
	if action == zActWrite && info.Size >= 0 && h.f0() == zCRESUM {
		if pi, err := os.Stat(part); err == nil && !pi.IsDir() && pi.Size() > 0 && pi.Size() < info.Size {
			l := pi.Size()
			remote, ok, err := s.zPrefixCRC(l)
			if err != nil {
				return res, false, err
			}
			if ok {
				if local, err := localPrefixCRC(part, l); err == nil && local == remote {
					start = l
				}
			}
		}
	}
	file, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return skip(OutcomeFailed, err.Error())
	}
	if err := file.Truncate(start); err != nil {
		_ = file.Close()
		return skip(OutcomeFailed, err.Error())
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		_ = file.Close()
		return skip(OutcomeFailed, err.Error())
	}
	res.ResumeFrom = start
	began := time.Now()
	s.startProgress("recv", res.Name, info.Size, start)
	w := bufio.NewWriterSize(file, 64*1024)
	got, fin, rerr := s.receiveData(w, start, &res)
	res.Elapsed = time.Since(began)
	res.Bytes = got - start
	flushErr := w.Flush()
	closeErr := file.Close()
	if rerr == nil && !fin {
		rerr = errors.Join(flushErr, closeErr)
		if rerr != nil {
			rerr = &zWriteError{err: rerr}
		}
	}
	var werr *zWriteError
	switch {
	case errors.As(rerr, &werr):
		res.Status, res.Reason = OutcomeFailed, werr.err.Error()
		if isDiskFull(werr.err) {
			// Keep the partial file as the resume point.
			_ = s.l.sendHex(zPosHeader(zFERR, 0))
			return res, false, fmt.Errorf("disk full writing %s; partial data kept in %s", target, part)
		}
		_ = os.Remove(part)
		return res, false, s.l.sendHex(zPosHeader(zSKIP, 0))
	case rerr != nil:
		res.Status, res.Reason = OutcomeFailed, rerr.Error()
		return res, false, rerr
	case fin:
		res.Status, res.Reason = OutcomeFailed, "sender ended the session before ZEOF"
		return res, true, nil
	}
	res.Size = got
	if err := commitPart(part, target, action == zActAppend); err != nil {
		res.Status, res.Reason = OutcomeFailed, err.Error()
		return res, false, nil
	}
	if !info.ModTime.IsZero() && action == zActWrite {
		_ = os.Chtimes(target, info.ModTime, info.ModTime)
	}
	res.Status = OutcomeOK
	if start > 0 {
		res.Status = OutcomeResumed
	}
	return res, false, nil
}

// zWriteError marks a local write failure, as opposed to a link failure.
type zWriteError struct{ err error }

func (e *zWriteError) Error() string { return e.err.Error() }

func commitPart(part, target string, appendTo bool) error {
	if !appendTo {
		return os.Rename(part, target)
	}
	src, err := os.Open(part)
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		_ = src.Close()
		return err
	}
	_, err = io.Copy(dst, src)
	_ = src.Close()
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Remove(part)
}

// sendAttn sends the sender's attention string before a ZRPOS. In it,
// 0xDD is a break (not supported here) and 0xDE is a one second pause.
func (s *zSession) sendAttn() {
	for _, b := range s.attn {
		switch b {
		case 0xdd:
		case 0xde:
			time.Sleep(time.Second)
		default:
			_ = s.l.write([]byte{b})
		}
	}
}

// receiveData reads ZDATA frames until ZEOF at the right position. Each
// error sends ZRPOS for the last good offset; the sender rewinds there.
func (s *zSession) receiveData(w io.Writer, start int64, res *ZFileResult) (int64, bool, error) {
	rx := start
	retries := 0
	buf := make([]byte, zRecvSubpacketMax)
	// rposOut: a ZRPOS is out and no ZDATA at rx has answered it yet.
	// Headers for other offsets and damaged headers seen meanwhile were
	// on the line before it and get no ZRPOS of their own; only a timeout
	// repeats it. Otherwise one error sets off a chain of ZRPOS.
	rposOut := true
	gotData := false
	lastRpos := time.Now()
	rpos := func() error {
		retries++
		if retries > zMaxRetries {
			return fmt.Errorf("giving up at offset %d after %d errors", rx, zMaxRetries)
		}
		s.sendAttn()
		rposOut, lastRpos = true, time.Now()
		return s.l.sendHex(zPosHeader(zRPOS, uint32(rx)))
	}
	// Offers repeated while a person chose where to save predate the
	// ZRPOS below.
	if err := s.dropQueued(zFILE); err != nil {
		return rx, false, err
	}
	if err := s.l.sendHex(zPosHeader(zRPOS, uint32(rx))); err != nil {
		return rx, false, err
	}
	for {
		h, err := s.l.readHeader(s.opts.DataWait)
		if errors.Is(err, errZTimeout) || errors.Is(err, errZBadHeader) {
			if errors.Is(err, errZBadHeader) {
				res.CRCErrors++
				s.report("")
				if rposOut {
					continue
				}
			}
			if err := rpos(); err != nil {
				return rx, false, err
			}
			continue
		}
		if err != nil {
			return rx, false, err
		}
		switch h.Type {
		case zDATA:
			if int64(h.pos()) != rx {
				if !rposOut {
					if err := rpos(); err != nil {
						return rx, false, err
					}
				}
				continue
			}
			rposOut, gotData = false, true
			crc32Mode := h.Format == zBIN32
			s.live.CRC32 = crc32Mode
		frame:
			for {
				data, end, err := s.l.readSubpacket(buf, zRecvSubpacketMax, crc32Mode, s.opts.DataWait)
				if errors.Is(err, errZPeerCancel) || errors.Is(err, errZLocalCancel) || errors.Is(err, errZLinkClosed) {
					return rx, false, err
				}
				if err != nil {
					res.CRCErrors++
					res.Retransmits++
					s.report("")
					if err := rpos(); err != nil {
						return rx, false, err
					}
					break frame
				}
				if len(data) > 0 {
					if _, err := w.Write(data); err != nil {
						return rx, false, &zWriteError{err: err}
					}
					rx += int64(len(data))
					retries = 0
					s.updateProgress(rx)
				}
				switch end {
				case zCRCW:
					if err := s.l.sendHex(zPosHeader(zACK, uint32(rx))); err != nil {
						return rx, false, err
					}
					break frame
				case zCRCQ:
					if err := s.l.sendHex(zPosHeader(zACK, uint32(rx))); err != nil {
						return rx, false, err
					}
				case zCRCE:
					break frame
				}
			}
		case zEOF:
			// An EOF at another offset was sent before our ZRPOS arrived.
			if int64(h.pos()) == rx {
				return rx, false, nil
			}
		case zFILE:
			// A repeated offer. Before any data, long after our ZRPOS, it
			// means that ZRPOS was lost; otherwise it is a stale copy.
			_, _, _ = s.l.readSubpacket(buf, zRecvSubpacketMax, h.Format == zBIN32, s.opts.DataWait)
			if !gotData && time.Since(lastRpos) >= zRepeatGap {
				if err := rpos(); err != nil {
					return rx, false, err
				}
			}
		case zABORT:
			return rx, false, s.peerGaveUp(h)
		case zFIN:
			return rx, true, nil
		case zSINIT:
			_, _, _ = s.l.readSubpacket(buf, zRecvSubpacketMax, h.Format == zBIN32, s.opts.DataWait)
			if err := s.l.sendHex(zPosHeader(zACK, 1)); err != nil {
				return rx, false, err
			}
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
