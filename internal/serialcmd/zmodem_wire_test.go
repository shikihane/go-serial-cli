package serialcmd

import (
	"io"
	"sync"
	"time"
)

// zWire is a test link between two ends. Each direction can delay, rate
// limit, corrupt and cut its bytes. Writes never block, so only the
// protocol window limits how much a sender has in flight.
type zWireConfig struct {
	latency time.Duration
	rate    int // bytes per second; 0 is unlimited
	// fault may change one chunk at its byte offset in the direction.
	fault func(offset int64, chunk []byte) []byte
	// cutAt closes the whole wire after this many bytes; 0 is never.
	cutAt int64
}

type zWireChunk struct {
	at   time.Time
	data []byte
}

type zWireEnd struct {
	out    chan zWireChunk
	in     *zQueue
	closer func()
}

func (e *zWireEnd) Read(b []byte) (int, error) { return e.in.read(b) }
func (e *zWireEnd) Close() error               { e.closer(); return nil }
func (e *zWireEnd) Write(b []byte) (int, error) {
	if e.in.isClosed() {
		return 0, io.ErrClosedPipe
	}
	defer func() { _ = recover() }()
	e.out <- zWireChunk{at: time.Now(), data: append([]byte(nil), b...)}
	return len(b), nil
}

type zQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func newZQueue() *zQueue {
	q := &zQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *zQueue) push(b []byte) {
	q.mu.Lock()
	q.buf = append(q.buf, b...)
	q.mu.Unlock()
	q.cond.Broadcast()
}

func (q *zQueue) read(b []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.buf) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(b, q.buf)
	q.buf = q.buf[n:]
	return n, nil
}

func (q *zQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

func (q *zQueue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

// newZWire returns the two ends. ab applies to bytes a writes.
func newZWire(ab, ba zWireConfig) (*zWireEnd, *zWireEnd) {
	qa, qb := newZQueue(), newZQueue()
	cha, chb := make(chan zWireChunk, 1<<16), make(chan zWireChunk, 1<<16)
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			qa.close()
			qb.close()
			close(cha)
			close(chb)
		})
	}
	a := &zWireEnd{out: cha, in: qa, closer: closeAll}
	b := &zWireEnd{out: chb, in: qb, closer: closeAll}
	go pumpZWire(cha, qb, ab, closeAll)
	go pumpZWire(chb, qa, ba, closeAll)
	return a, b
}

func pumpZWire(ch chan zWireChunk, dst *zQueue, cfg zWireConfig, closeAll func()) {
	var offset int64
	var busyUntil time.Time
	for c := range ch {
		sent, chunk := c.at, c.data
		if cfg.fault != nil {
			chunk = cfg.fault(offset, chunk)
		}
		offset += int64(len(chunk))
		if cfg.cutAt > 0 && offset >= cfg.cutAt {
			keep := len(chunk) - int(offset-cfg.cutAt)
			dst.push(chunk[:max(0, keep)])
			closeAll()
			return
		}
		at := sent.Add(cfg.latency)
		if cfg.rate > 0 {
			start := busyUntil
			if start.Before(sent) {
				start = sent
			}
			busyUntil = start.Add(time.Duration(len(chunk)) * time.Second / time.Duration(cfg.rate))
			if busyUntil.Add(cfg.latency).After(at) {
				at = busyUntil.Add(cfg.latency)
			}
		}
		if d := time.Until(at); d > 0 {
			time.Sleep(d)
		}
		dst.push(chunk)
	}
}
