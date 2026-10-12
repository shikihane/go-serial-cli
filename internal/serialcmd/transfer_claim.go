package serialcmd

import (
	"bytes"
	"net"
	"sync"
)

// A file transfer declares itself by sending transferClaimPreamble as the
// first bytes on a control connection. The worker strips the preamble and
// answers that connection alone with transferClaimOK or transferClaimBusy, in
// line with the serial output it already streams. While a claim is held,
// writes from every other source are dropped and serial output goes to the
// owner only. The claim ends when the owning connection closes.
const (
	transferClaimPreamble = "\x00sio:transfer-claim\x00"
	transferClaimOK       = "\x00sio:transfer-ok\x00"
	transferClaimBusy     = "\x00sio:transfer-busy\x00"

	// Plain text for the other clients of a claimed session.
	transferClaimNotice   = "\r\n[sio: file transfer in progress, input dropped]\r\n"
	transferReleaseNotice = "\r\n[sio: file transfer ended]\r\n"
)

// portClaim records the one control connection allowed to write during a
// transfer. The zero value has no owner.
type portClaim struct {
	mu    sync.Mutex
	owner net.Conn
}

func (c *portClaim) take(conn net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owner != nil && c.owner != conn {
		return false
	}
	c.owner = conn
	return true
}

func (c *portClaim) release(conn net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owner == conn {
		c.owner = nil
	}
}

// current returns the owner, or nil when no transfer holds the port.
func (c *portClaim) current() net.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owner
}

// allows reports whether conn may write now. A nil conn stands for a
// source that can never hold a claim, such as a shared virtual port.
func (c *portClaim) allows(conn net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owner == nil || (conn != nil && c.owner == conn)
}

// readClaim reads the first bytes of a control connection. It reads more
// only while the bytes so far could still be the preamble, so a normal
// client is forwarded after its first read. It returns the bytes to forward
// and whether the client sent the preamble.
func readClaim(conn net.Conn, buf []byte) ([]byte, bool, error) {
	preamble := []byte(transferClaimPreamble)
	var acc []byte
	for {
		n, err := conn.Read(buf)
		acc = append(acc, buf[:n]...)
		if bytes.HasPrefix(acc, preamble) {
			return acc[len(preamble):], true, err
		}
		if !bytes.HasPrefix(preamble, acc) || err != nil {
			return acc, false, err
		}
	}
}

// answerClaim takes or refuses the claim for conn and tells the client.
func answerClaim(claim *portClaim, conn net.Conn) bool {
	if !claim.take(conn) {
		_, _ = conn.Write([]byte(transferClaimBusy))
		return false
	}
	_, _ = conn.Write([]byte(transferClaimOK))
	return true
}
