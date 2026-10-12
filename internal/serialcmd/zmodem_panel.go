package serialcmd

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The shell transfer panel: two status lines at the bottom of the
// terminal, redrawn in place with plain ANSI sequences while a transfer
// runs, then the session summary until the person presses Enter.

const (
	shellPanelTick = 250 * time.Millisecond
	// shellPanelRateWindow is how far back the rate looks.
	shellPanelRateWindow = 3 * time.Second
	shellPanelSampleGap  = 100 * time.Millisecond
	// shellPanelHoldMax bounds the device output held while the summary
	// is shown; older bytes are dropped first.
	shellPanelHoldMax = 1 << 20
	shellPanelBarMin  = 10
	shellPanelBarMax  = 30
	// shellPanelWidth is used when the terminal does not say.
	shellPanelWidth = 80
)

const (
	panelArrowSend = "\u2191" // up arrow: to the device
	panelArrowRecv = "\u2193" // down arrow: from the device
	// panelHint: "Press Enter to return to the shell" in Chinese (an hui
	// che fan hui shell), then in English.
	panelHint = "\u6309\u56de\u8f66\u8fd4\u56de shell / Press Enter to return to the shell"
)

// panelLine is one row of the panel. Rows never reach the last column,
// so the terminal does not wrap them.
type panelLine struct {
	text   string
	middle bool // shorten in the middle, keeping the end (file names)
	// parts, when set, are joined instead of text, leaving out the
	// trailing ones that do not fit.
	parts []string
	hint  bool // drawn in reverse video
}

type panelSample struct {
	at   time.Time
	done int64
}

type shellPanel struct {
	out   io.Writer
	width func() int

	// smu guards the progress state the transfer goroutine updates; it
	// never waits for the terminal.
	smu     sync.Mutex
	prog    ZProgress
	have    bool
	phaseAt time.Time
	samples []panelSample
	cur     panelSample

	// mu serializes everything written to the terminal.
	mu       sync.Mutex
	rows     []int // display width of each row on screen
	drawnAt  int   // terminal width the rows were drawn at
	paused   bool
	summary  []panelLine
	holdText bool
	closed   bool
	held     []byte
	dropped  int

	kick     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func newShellPanel(out io.Writer, width func() int) *shellPanel {
	p := &shellPanel{
		out:   out,
		width: width,
		kick:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go p.loop()
	return p
}

func (p *shellPanel) loop() {
	defer close(p.done)
	tick := time.NewTicker(shellPanelTick)
	defer tick.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-tick.C:
		case <-p.kick:
		}
		p.mu.Lock()
		if !p.paused && !p.closed && p.summary == nil {
			p.drawLocked()
		}
		p.mu.Unlock()
	}
}

func (p *shellPanel) stopTicker() {
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done
}

// update is the transfer's progress callback.
func (p *shellPanel) update(z ZProgress) {
	now := time.Now()
	p.smu.Lock()
	redraw := !p.have || z.Phase != p.prog.Phase
	if !p.have || z.Phase != p.prog.Phase || z.File != p.prog.File {
		p.phaseAt = now
	}
	if !p.have || z.File != p.prog.File || z.Name != p.prog.Name || z.Done < p.cur.done || z.Phase != ZPhaseData {
		p.samples = p.samples[:0]
	}
	p.cur = panelSample{at: now, done: z.Done}
	if z.Phase == ZPhaseData {
		if n := len(p.samples); n == 0 || now.Sub(p.samples[n-1].at) >= shellPanelSampleGap {
			p.samples = append(p.samples, p.cur)
		}
	}
	p.prog, p.have = z, true
	p.smu.Unlock()
	if z.Phase == ZPhaseFinish {
		// The device's result line and prompt follow; they belong after
		// the summary.
		p.mu.Lock()
		p.holdText = true
		p.mu.Unlock()
	}
	if redraw {
		select {
		case p.kick <- struct{}{}:
		default:
		}
	}
}

// view returns the progress, its rate in bytes per second and how long
// the current phase has lasted.
func (p *shellPanel) view(now time.Time) (ZProgress, float64, time.Duration, bool) {
	p.smu.Lock()
	defer p.smu.Unlock()
	for len(p.samples) > 1 && now.Sub(p.samples[0].at) > shellPanelRateWindow {
		p.samples = p.samples[1:]
	}
	rate := 0.0
	if len(p.samples) > 0 {
		first := p.samples[0]
		if now.Sub(first.at) > shellPanelRateWindow {
			// Nothing moved for the whole window: a stall.
			first = p.cur
		}
		if d := now.Sub(first.at).Seconds(); d > 0.2 {
			rate = float64(p.cur.done-first.done) / d
		}
	}
	return p.prog, rate, now.Sub(p.phaseAt), p.have
}

// Write prints transfer log lines above the panel.
func (p *shellPanel) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(b), p.aboveLocked(b)
}

// deviceOutput shows device bytes: above the panel while the transfer
// runs, held back once its summary is due, straight through once the
// panel is closed.
func (p *shellPanel) deviceOutput(b []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.closed:
		_, err := p.out.Write(b)
		return err
	case p.holdText || p.summary != nil:
		p.held = append(p.held, b...)
		if over := len(p.held) - shellPanelHoldMax; over > 0 {
			p.held = append(p.held[:0:0], p.held[over:]...)
			p.dropped += over
		}
		return nil
	}
	return p.aboveLocked(b)
}

type panelText struct{ p *shellPanel }

func (t panelText) Write(b []byte) (int, error) { return len(b), t.p.deviceOutput(b) }

func (p *shellPanel) aboveLocked(b []byte) error {
	if p.closed || p.paused {
		_, err := p.out.Write(b)
		return err
	}
	var buf strings.Builder
	p.eraseLocked(&buf, p.termWidth())
	buf.Write(b)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		buf.WriteString("\r\n")
	}
	if _, err := io.WriteString(p.out, buf.String()); err != nil {
		return err
	}
	if p.summary != nil || p.have {
		p.drawLocked()
	}
	return nil
}

// pause clears the panel while an inline prompt uses the input line.
func (p *shellPanel) pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	var buf strings.Builder
	p.eraseLocked(&buf, p.termWidth())
	_, _ = io.WriteString(p.out, buf.String())
	p.paused = true
}

func (p *shellPanel) resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = false
	if !p.closed {
		p.drawLocked()
	}
}

// finish replaces the progress lines with the summary.
func (p *shellPanel) finish(lines []panelLine) {
	p.stopTicker()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = false
	p.summary = lines
	p.drawLocked()
}

// close clears the panel, prints record and then the device output held
// meanwhile. Later writes go straight through.
func (p *shellPanel) close(record string) {
	p.stopTicker()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	var buf strings.Builder
	p.eraseLocked(&buf, p.termWidth())
	buf.WriteString(record)
	if p.dropped > 0 {
		fmt.Fprintf(&buf, "[sio: %d bytes of device output dropped while the summary was shown]\r\n", p.dropped)
	}
	buf.Write(p.held)
	_, _ = io.WriteString(p.out, buf.String())
	p.held, p.dropped = nil, 0
	p.closed = true
}

func (p *shellPanel) termWidth() int {
	if p.width != nil {
		if w := p.width(); w > 0 {
			return w
		}
	}
	return shellPanelWidth
}

// eraseLocked moves to the first panel row and clears to the end of the
// screen. Rows drawn at a wider terminal may have been reflowed into
// more rows since.
func (p *shellPanel) eraseLocked(buf *strings.Builder, width int) {
	if len(p.rows) == 0 {
		return
	}
	up := 0
	for i, w := range p.rows {
		n := 1
		if width < p.drawnAt && w > width {
			n = (w + width - 1) / width
		}
		if i == len(p.rows)-1 {
			n--
		}
		up += n
	}
	buf.WriteString("\r")
	if up > 0 {
		fmt.Fprintf(buf, "\x1b[%dA", up)
	}
	buf.WriteString("\x1b[J")
	p.rows = nil
}

func (p *shellPanel) drawLocked() {
	width := p.termWidth()
	lines := p.summary
	if lines == nil {
		prog, rate, waited, ok := p.view(time.Now())
		if !ok {
			return
		}
		l1, l2 := panelProgressLines(prog, rate, waited, width)
		lines = []panelLine{{text: l1}, {text: l2}}
	}
	var buf strings.Builder
	p.eraseLocked(&buf, width)
	rows := make([]int, 0, len(lines))
	for i, l := range lines {
		if i > 0 {
			buf.WriteString("\r\n")
		}
		text := fitEnd(l.text, width-1)
		switch {
		case l.parts != nil:
			text = joinFit(l.parts, width-1)
		case l.middle:
			text = fitMiddle(l.text, width-1)
		}
		if l.hint {
			buf.WriteString("\x1b[7m")
			buf.WriteString(text)
			buf.WriteString("\x1b[0m")
		} else {
			buf.WriteString(text)
		}
		rows = append(rows, displayWidth(text))
	}
	if _, err := io.WriteString(p.out, buf.String()); err != nil {
		return
	}
	p.rows, p.drawnAt = rows, width
}

// ---- layout ----

func panelArrow(send bool) string {
	if send {
		return panelArrowSend
	}
	return panelArrowRecv
}

func panelPhaseText(z ZProgress) string {
	switch z.Phase {
	case ZPhaseHandshake:
		switch {
		case z.Send:
			return "waiting for the device to start receiving"
		case z.File > 0:
			return "waiting for the next file"
		}
		return "waiting for the device to send"
	case ZPhasePick:
		if z.Send {
			return "choose the file(s) to send"
		}
		return "choose where to save"
	case ZPhaseOffer:
		if z.Send {
			return "waiting for the device to accept the file"
		}
		return "preparing the file"
	case ZPhaseCheck:
		if z.Send {
			return "device is comparing its copy (CRC)"
		}
		return "comparing with the existing file (CRC)"
	case ZPhaseFinish:
		return "finishing"
	}
	return z.Phase
}

// panelProgressLines lays out the two progress rows for width columns.
func panelProgressLines(z ZProgress, rate float64, waited time.Duration, width int) (string, string) {
	avail := width - 1
	head := panelArrow(z.Send) + " "
	name := z.Name
	if name == "" {
		name = "-"
	}
	var stats []string
	if z.Phase == ZPhaseData {
		r := "-- KB/s"
		if rate > 0 {
			r = formatBytes(int64(rate)) + "/s"
		}
		stats = append(stats, r)
		if z.Total > 0 {
			eta := "--:--"
			if rate > 0 {
				left := time.Duration(float64(max(z.Total-z.Done, 0)) / rate * float64(time.Second))
				eta = formatClock(left)
			}
			stats = append(stats, "ETA "+eta)
		}
	} else {
		stats = append(stats, "waited "+formatClock(waited))
	}
	if z.File > 0 || z.Phase == ZPhaseData {
		window := "-"
		if z.Window > 0 {
			window = fmt.Sprint(z.Window)
		}
		crc := "CRC16"
		if z.CRC32 {
			crc = "CRC32"
		}
		file := fmt.Sprintf("file %d", z.File)
		if z.Files > 0 {
			file += fmt.Sprintf("/%d", z.Files)
		}
		stats = append(stats,
			fmt.Sprintf("retrans %d", z.Retransmits),
			fmt.Sprintf("CRC err %d", z.CRCErrors),
			"W "+window, crc, file)
	}
	line2 := joinFit(stats, avail)

	if z.Phase != ZPhaseData {
		status := "  " + panelPhaseText(z)
		if z.Name == "" {
			return fitEnd(head+panelPhaseText(z), avail), line2
		}
		room := avail - displayWidth(head) - displayWidth(status)
		if room < 8 {
			return fitEnd(head+fitMiddle(name, max(avail-displayWidth(head), 1))+status, avail), line2
		}
		return head + fitMiddle(name, room) + status, line2
	}
	if z.Total < 0 {
		// Unknown size: bytes only, no bar, percent or ETA.
		tail := "  " + formatBytes(z.Done)
		room := avail - displayWidth(head) - displayWidth(tail)
		return fitEnd(head+fitMiddle(name, max(room, 1))+tail, avail), line2
	}
	pct := 100
	if z.Total > 0 {
		pct = int(min(max(z.Done, 0)*100/z.Total, 100))
	}
	pctText := fmt.Sprintf("%3d%%", pct)
	tail := "  " + pctText + "  " + formatBytes(z.Done) + "/" + formatBytes(z.Total)
	nameW := displayWidth(name)
	room := avail - displayWidth(head) - displayWidth(tail)
	if room >= min(nameW, 12)+2+shellPanelBarMin+2 {
		shown := name
		if nameW > room-2-(shellPanelBarMin+2) {
			shown = fitMiddle(name, room-2-(shellPanelBarMin+2))
		}
		inner := min(room-displayWidth(shown)-2-2, shellPanelBarMax)
		filled := inner * pct / 100
		bar := "[" + strings.Repeat("#", filled) + strings.Repeat("-", inner-filled) + "]"
		return head + shown + "  " + bar + tail, line2
	}
	if room < 8 {
		tail = "  " + pctText
		room = avail - displayWidth(head) - displayWidth(tail)
	}
	return fitEnd(head+fitMiddle(name, max(room, 1))+tail, avail), line2
}

// panelSummary lays out the summary rows of a finished session.
func panelSummary(send bool, res ZModemResult, err error) []panelLine {
	outcome := res.Outcome
	if outcome == "" {
		outcome = OutcomeFailed
	}
	var names []string
	var bytes int64
	var elapsed time.Duration
	var retrans, crcErrs int
	var resumed []string
	for _, f := range res.Files {
		name := f.Name
		if !send && f.Path != "" && f.Path != f.Name && f.Status != OutcomeFailed {
			name += " -> " + f.Path
		}
		if len(res.Files) > 1 {
			name += " " + f.Status
		}
		names = append(names, name)
		bytes += f.Bytes
		elapsed += f.Elapsed
		retrans += f.Retransmits
		crcErrs += f.CRCErrors
		if f.ResumeFrom > 0 || len(res.Files) == 1 {
			resumed = append(resumed, fmt.Sprint(f.ResumeFrom))
		}
	}
	line1 := panelArrow(send) + " " + outcome
	if len(names) > 0 {
		line1 += "  " + strings.Join(names, ", ")
	}
	// Partial and refused batches name each file's status already; a
	// plain local cancel needs no reason.
	if err != nil && (outcome == OutcomeFailed || outcome == OutcomeCancelled && err != ErrTransferCancelled) {
		line1 += "  (" + err.Error() + ")"
	}
	if len(resumed) == 0 {
		resumed = []string{"0"}
	}
	stats := []string{
		fmt.Sprintf("%d bytes (%s) in %s", bytes, formatBytes(bytes), formatElapsed(elapsed)),
		"avg " + formatBytes(int64(rateOf(bytes, elapsed))) + "/s",
		fmt.Sprintf("retrans %d", retrans),
		fmt.Sprintf("CRC err %d", crcErrs),
		"resumed from " + strings.Join(resumed, ","),
	}
	return []panelLine{
		{text: line1, middle: true},
		{parts: stats},
		{text: panelHint, hint: true},
	}
}

func rateOf(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) / d.Seconds()
}

func formatBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
}

// formatClock prints m:ss, or h:mm:ss from an hour on.
func formatClock(d time.Duration) string {
	s := int64(d.Round(time.Second) / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func formatElapsed(d time.Duration) string {
	if d < time.Minute {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// joinFit joins parts with two spaces, leaving out the trailing ones that
// do not fit.
func joinFit(parts []string, width int) string {
	out := ""
	for i, part := range parts {
		next := part
		if i > 0 {
			next = out + "  " + part
		}
		if displayWidth(next) > width {
			if i == 0 {
				return fitEnd(part, width)
			}
			break
		}
		out = next
	}
	return out
}

// ---- display width ----

// runeWidth is the number of terminal cells r takes. East Asian wide
// characters take two; so do the arrows, which CJK consoles draw wide.
func runeWidth(r rune) int {
	switch {
	case r < 0x20 || r == 0x7f:
		return 0
	case r < 0x1100:
		return 1
	case r <= 0x115f,
		r >= 0x2190 && r <= 0x21ff,
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f,
		r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe30 && r <= 0xfe4f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1f64f,
		r >= 0x1f900 && r <= 0x1f9ff,
		r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// fitEnd cuts s to width cells.
func fitEnd(s string, width int) string {
	if displayWidth(s) <= width {
		return s
	}
	w := 0
	for i, r := range s {
		if w+runeWidth(r) > width {
			return s[:i]
		}
		w += runeWidth(r)
	}
	return s
}

// fitMiddle shortens s to width cells by replacing its middle with "...",
// keeping the end, where a file name has its extension.
func fitMiddle(s string, width int) string {
	if displayWidth(s) <= width {
		return s
	}
	if width <= 3 {
		return fitEnd(s, width)
	}
	keep := width - 3
	headW := keep / 2
	tailW := keep - headW
	head := fitEnd(s, headW)
	tail := ""
	w := 0
	for i := len(s); i > 0; {
		r, size := utf8.DecodeLastRuneInString(s[:i])
		if w+runeWidth(r) > tailW {
			break
		}
		w += runeWidth(r)
		i -= size
		tail = s[i:]
	}
	return head + "..." + tail
}
