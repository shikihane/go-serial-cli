package serialcmd

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// A shell can lend the physical serial port to another program. The request
// travels as the first bytes of a control connection, like the transfer
// claim: the worker closes the port and stops reading it until the shell asks
// for it back. Answers are NUL-framed tokens so device output that reaches
// the control connection before the answer cannot be mistaken for it.
const (
	portReleasePreamble = "\x00sio:port-release\x00"
	portAcquirePreamble = "\x00sio:port-acquire\x00"
	portLeaseOK         = "\x00sio:port-ok\x00"
	portLeaseError      = "\x00sio:port-error\x00"

	// Plain text for clients of a session whose port is lent out.
	portReleasedNotice = "\r\n[sio: serial port is released for another program, input dropped]\r\n"
)

// portLeaseErrorLimit bounds one answer line so a noisy driver message cannot
// flood the client.
const portLeaseErrorLimit = 200

// A lease read returns after this long even with no data, so a release can
// close the port while no read is pending. Windows keeps a handle with pending
// overlapped I/O open, which would keep the port claimed by another program's
// point of view.
const portLeaseReadTimeout = 100 * time.Millisecond

// portLeaseReleaseWait bounds how long a release waits for the read loop to
// close the port.
const portLeaseReleaseWait = 2 * time.Second

var errPortReleased = errors.New("serial port is released for another program")

// portLease owns the physical port of a session server. A release closes the
// handle so another program can open it; an acquire opens it again with the
// session's port name and baud rate. The read loop closes a released port
// itself, after its last read returned, and signals idle.
type portLease struct {
	mu       sync.Mutex
	cond     *sync.Cond
	port     SerialPort
	name     string
	baud     int
	open     func(portName string, baud int) (SerialPort, error)
	released bool
	closed   bool
	idle     chan struct{}
	idleDone bool
	onChange func(released bool)
}

func newPortLease(port SerialPort, name string, baud int, open func(string, int) (SerialPort, error), onChange func(bool)) *portLease {
	lease := &portLease{port: port, name: name, baud: baud, open: open, onChange: onChange, idle: make(chan struct{})}
	lease.cond = sync.NewCond(&lease.mu)
	setLeaseReadTimeout(port)
	return lease
}

// setLeaseReadTimeout makes reads return now and then. Ports that do not
// support it keep blocking reads; a release then waits for the read in flight
// to finish on its own, or reports that the port stayed busy.
func setLeaseReadTimeout(port SerialPort) {
	if port == nil {
		return
	}
	if setter, ok := port.(interface{ SetReadTimeout(time.Duration) error }); ok {
		_ = setter.SetReadTimeout(portLeaseReadTimeout)
	}
}

// waitPort blocks while the port is released and returns the port to read
// from, or nil once the lease is closed. When it finds a released port still
// open, it closes it: the read loop is between reads here, so closing it
// releases the handle at once.
func (l *portLease) waitPort() SerialPort {
	l.mu.Lock()
	for {
		if l.closed {
			l.mu.Unlock()
			return nil
		}
		if l.released {
			if l.port != nil {
				port := l.port
				l.port = nil
				l.mu.Unlock()
				_ = port.Close()
				l.finishIdle()
				l.mu.Lock()
				continue
			}
			l.cond.Wait()
			continue
		}
		port := l.port
		l.mu.Unlock()
		return port
	}
}

// current returns the port when it is open, or nil while it is released.
func (l *portLease) current() SerialPort {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.released {
		return nil
	}
	return l.port
}

// retire closes port when a release was asked for while the read was in
// flight. done reports that it closed the port; closed reports that the lease
// is shutting down and the read loop should return.
func (l *portLease) retire(port SerialPort) (done bool, closed bool) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return false, true
	}
	if !l.released || l.port != port {
		l.mu.Unlock()
		return false, false
	}
	l.port = nil
	l.mu.Unlock()
	_ = port.Close()
	l.finishIdle()
	return true, false
}

// finishIdle tells a waiting release that the port handle is closed.
func (l *portLease) finishIdle() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finishIdleLocked()
}

func (l *portLease) finishIdleLocked() {
	if l.idleDone {
		return
	}
	l.idleDone = true
	close(l.idle)
}

// release lends the port to another program. It reports false when the port
// was already released, so the caller can tell a no-op from the first release.
func (l *portLease) release() (bool, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return false, errors.New("session is closing")
	}
	if l.released {
		l.mu.Unlock()
		return false, nil
	}
	l.released = true
	idle := l.idle
	l.cond.Broadcast()
	l.mu.Unlock()

	select {
	case <-idle:
	case <-time.After(portLeaseReleaseWait):
		return true, errors.New("serial port is still reading; try again or stop the session")
	}
	l.notify(true)
	return true, nil
}

// acquire opens the port again. It reports false when the port was already
// open, so the caller can tell a no-op from a reopen.
func (l *portLease) acquire() (bool, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return false, errors.New("session is closing")
	}
	if !l.released {
		l.mu.Unlock()
		return false, nil
	}
	name, baud, open := l.name, l.baud, l.open
	l.mu.Unlock()

	port, err := open(name, baud)
	if err != nil {
		return false, err
	}
	setLeaseReadTimeout(port)

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		_ = port.Close()
		return false, errors.New("session is closing")
	}
	l.port = port
	l.released = false
	l.idle = make(chan struct{})
	l.idleDone = false
	l.cond.Broadcast()
	l.mu.Unlock()
	l.notify(false)
	return true, nil
}

func (l *portLease) close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	port := l.port
	l.port = nil
	l.finishIdleLocked()
	l.cond.Broadcast()
	l.mu.Unlock()
	if port != nil {
		_ = port.Close()
	}
}

func (l *portLease) notify(released bool) {
	if l.onChange != nil {
		l.onChange(released)
	}
}

// controlRequest is the kind of request a control connection starts with.
type controlRequest int

const (
	requestData controlRequest = iota
	requestTransferClaim
	requestPortRelease
	requestPortAcquire
)

type controlPreamble struct {
	preamble string
	request  controlRequest
}

var sessionServerRequests = []controlPreamble{
	{preamble: transferClaimPreamble, request: requestTransferClaim},
	{preamble: portReleasePreamble, request: requestPortRelease},
	{preamble: portAcquirePreamble, request: requestPortAcquire},
}

// readRequest reads the first bytes of a control connection. It reads more
// only while the bytes so far could still be one of the preambles, so a normal
// client is forwarded after its first read. It returns the bytes to forward
// and the request the client started with.
func readRequest(conn net.Conn, buf []byte, requests []controlPreamble) ([]byte, controlRequest, error) {
	var acc []byte
	for {
		n, err := conn.Read(buf)
		acc = append(acc, buf[:n]...)
		partial := false
		for _, request := range requests {
			preamble := []byte(request.preamble)
			if bytes.HasPrefix(acc, preamble) {
				return acc[len(preamble):], request.request, err
			}
			if bytes.HasPrefix(preamble, acc) {
				partial = true
			}
		}
		if !partial || err != nil {
			return acc, requestData, err
		}
	}
}

// answerPortRequest writes the worker's answer to a port lease request.
func answerPortRequest(conn net.Conn, err error) {
	if err == nil {
		_, _ = conn.Write([]byte(portLeaseOK))
		return
	}
	text := oneLineText(err.Error(), portLeaseErrorLimit)
	if text == "" {
		text = "port request failed"
	}
	_, _ = conn.Write(append(append([]byte(portLeaseError), text...), 0))
}

func oneLineText(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > limit {
		text = text[:limit]
	}
	return text
}

// ReleaseSessionPort asks the session worker to close its serial port so
// another program can use it. The worker keeps running: sio read, the cache,
// and the shell connection stay in place, and AcquireSessionPort takes the
// port back.
func ReleaseSessionPort(address string) error {
	return leaseRequest(address, portReleasePreamble)
}

func AcquireSessionPort(address string) error {
	return leaseRequest(address, portAcquirePreamble)
}

// leaseRequest sends one port lease request and waits for the worker's
// NUL-framed answer, skipping any serial output a broadcast may have put in
// front of it.
func leaseRequest(address string, preamble string) error {
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(preamble)); err != nil {
		return err
	}
	return readLeaseAnswer(conn)
}

func readLeaseAnswer(conn net.Conn) error {
	reader := bufio.NewReaderSize(io.LimitReader(conn, 64*1024), 512)
	ok := []byte(portLeaseOK)
	failed := []byte(portLeaseError)
	tokenLen := len(ok)
	if len(failed) > tokenLen {
		tokenLen = len(failed)
	}
	window := make([]byte, 0, tokenLen)
	for {
		ch, err := reader.ReadByte()
		if err != nil {
			return errors.New("session worker did not answer the port request; restart it with sio stop and sio open")
		}
		window = append(window, ch)
		if len(window) > tokenLen {
			window = window[1:]
		}
		switch {
		case bytes.HasSuffix(window, ok):
			return nil
		case bytes.HasSuffix(window, failed):
			text, err := reader.ReadString(0)
			if err != nil {
				return errors.New("session worker did not finish its port request answer")
			}
			text = strings.TrimSuffix(text, "\x00")
			if text == "" {
				return errors.New("session worker refused the port request")
			}
			return errors.New(text)
		}
	}
}
