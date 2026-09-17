package serialcmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

const (
	txOverlayPollInterval = 100 * time.Millisecond
	txOverlayIdleFlush    = 100 * time.Millisecond
	txOverlayLineLimit    = 64
	txOverlayPendingLimit = 4096
)

type ShellStreamOptions struct {
	Address          string
	Input            io.Reader
	Output           io.Writer
	TxCachePath      string
	TxCacheIndexPath string
}

// StreamShell attaches an interactive shell to a session worker and, when the
// session records TX captures, overlays other senders' traffic as tagged lines.
// The shell's own sends are filtered out by connection address so typed input
// is not shown twice.
func StreamShell(opts ShellStreamOptions) error {
	conn, err := net.Dial("tcp", opts.Address)
	if err != nil {
		return err
	}
	defer conn.Close()
	output := opts.Output
	if output == nil {
		output = io.Discard
	}
	if opts.TxCachePath != "" {
		indexPath := opts.TxCacheIndexPath
		if indexPath == "" {
			indexPath = CacheIndexPath(opts.TxCachePath)
		}
		selfSource := "tcp:" + conn.LocalAddr().String()
		stop := make(chan struct{})
		defer close(stop)
		go tailTXOverlay(stop, opts.TxCachePath, indexPath, selfSource, output)
	}
	errCh := make(chan error, 2)
	if opts.Input != nil {
		go func() {
			errCh <- copyInputToPort(opts.Input, conn, opts.Address)
		}()
	}
	go func() {
		_, err := io.Copy(output, conn)
		errCh <- normalizeCopyError(err)
	}()
	return <-errCh
}

func tailTXOverlay(stop <-chan struct{}, cachePath string, indexPath string, selfSource string, out io.Writer) {
	var indexOffset int64
	if info, err := os.Stat(indexPath); err == nil {
		indexOffset = info.Size()
	}
	assemblers := map[string]*txLineAssembler{}
	ticker := time.NewTicker(txOverlayPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		entries, newOffset := readCacheIndexFrom(indexPath, indexOffset)
		indexOffset = newOffset
		for _, entry := range entries {
			if entry.Source == "" || entry.Source == selfSource {
				continue
			}
			data := readCacheRangeBytes(cachePath, entry.Offset, entry.Length)
			if len(data) == 0 {
				continue
			}
			assembler := assemblers[entry.Source]
			if assembler == nil {
				assembler = &txLineAssembler{source: entry.Source}
				assemblers[entry.Source] = assembler
			}
			assembler.feed(data, out)
		}
		for _, assembler := range assemblers {
			assembler.flushIfIdle(out)
		}
	}
}

// readCacheIndexFrom parses complete JSONL entries appended after offset and
// returns the offset just past the last complete line, so a partially written
// trailing line is re-read on the next poll.
func readCacheIndexFrom(indexPath string, offset int64) ([]cacheIndexEntry, int64) {
	f, err := os.Open(indexPath)
	if err != nil {
		return nil, offset
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset
	}
	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return nil, offset
	}
	lastNewline := bytes.LastIndexByte(data, '\n')
	if lastNewline < 0 {
		return nil, offset
	}
	data = data[:lastNewline+1]
	var entries []cacheIndexEntry
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var entry cacheIndexEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.Length <= 0 {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, offset + int64(lastNewline+1)
}

func readCacheRangeBytes(cachePath string, offset int64, length int64) []byte {
	f, err := os.Open(cachePath)
	if err != nil {
		return nil
	}
	defer f.Close()
	data := make([]byte, length)
	n, err := f.ReadAt(data, offset)
	if n <= 0 && err != nil {
		return nil
	}
	return data[:n]
}

// txLineAssembler buffers one sender's byte stream and emits complete display
// lines. Partial lines flush after an idle window so senders that never write
// a newline still surface, and oversized buffers flush early to bound memory.
type txLineAssembler struct {
	source   string
	pending  []byte
	lastFeed time.Time
}

func (a *txLineAssembler) feed(data []byte, out io.Writer) {
	a.pending = append(a.pending, data...)
	a.lastFeed = time.Now()
	for {
		idx := bytes.IndexByte(a.pending, '\n')
		if idx < 0 {
			break
		}
		line := a.pending[:idx]
		a.pending = append([]byte(nil), a.pending[idx+1:]...)
		a.emit(line, out)
	}
	if len(a.pending) > txOverlayPendingLimit {
		line := a.pending
		a.pending = nil
		a.emit(line, out)
	}
}

func (a *txLineAssembler) flushIfIdle(out io.Writer) {
	if len(a.pending) == 0 || time.Since(a.lastFeed) < txOverlayIdleFlush {
		return
	}
	line := a.pending
	a.pending = nil
	a.emit(line, out)
}

func (a *txLineAssembler) emit(line []byte, out io.Writer) {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if len(line) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "[%s→] %s\n", a.source, escapeDisplayLine(line, txOverlayLineLimit))
}
