package serialcmd

import (
	"io"
	"sync"
	"time"
)

// asyncCapture moves cache writes off the forwarding path. A file
// transfer waits on acknowledgements, so a disk write in front of the forward
// adds straight to each packet. Chunks keep their arrival time and order.
// When the queue is full, Write blocks rather than drop log data.
type asyncCapture struct {
	dst  io.Writer
	ch   chan TimedChunk
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

const asyncCaptureQueue = 4096

func newAsyncCapture(dst io.Writer) *asyncCapture {
	c := &asyncCapture{dst: dst, ch: make(chan TimedChunk, asyncCaptureQueue), done: make(chan struct{})}
	go c.run()
	return c
}

func (c *asyncCapture) run() {
	defer close(c.done)
	for chunk := range c.ch {
		if _, err := WriteTimedChunks(c.dst, []TimedChunk{chunk}); err != nil {
			c.mu.Lock()
			if c.err == nil {
				c.err = err
			}
			c.mu.Unlock()
		}
	}
}

// Write queues a copy of data stamped with the current time. It returns
// the first error the background writer has hit, if any.
func (c *asyncCapture) Write(data []byte) (int, error) {
	return c.WriteChunk(TimedChunk{At: time.Now().Local(), Data: append([]byte(nil), data...)})
}

func (c *asyncCapture) WriteChunk(chunk TimedChunk) (int, error) {
	if len(chunk.Data) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	c.ch <- chunk
	return len(chunk.Data), nil
}

// Close flushes the queue. Call it before closing dst.
func (c *asyncCapture) Close() {
	c.once.Do(func() {
		close(c.ch)
		<-c.done
	})
}
