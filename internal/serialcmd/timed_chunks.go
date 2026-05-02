package serialcmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type TimedChunk struct {
	At   time.Time
	Data []byte
}

type cacheIndexEntry struct {
	At     time.Time `json:"at"`
	Offset int64     `json:"offset"`
	Length int64     `json:"length"`
}

type timedCacheWriter struct {
	file  *os.File
	index *os.File
}

type closerFunc func()

func (f closerFunc) Close() error {
	f()
	return nil
}

func OpenTimedCacheWriter(cachePath string, indexPath string) (io.Writer, func(), error) {
	if cachePath == "" {
		return io.Discard, func() {}, nil
	}
	cache, err := openAppendFile(cachePath)
	if err != nil {
		return nil, func() {}, err
	}
	if indexPath == "" {
		return cache, func() { _ = cache.Close() }, nil
	}
	index, err := openAppendFile(indexPath)
	if err != nil {
		_ = cache.Close()
		return nil, func() {}, err
	}
	writer := &timedCacheWriter{file: cache, index: index}
	return writer, func() {
		_ = index.Close()
		_ = cache.Close()
	}, nil
}

func (w *timedCacheWriter) Write(data []byte) (int, error) {
	return w.WriteChunk(TimedChunk{At: time.Now().Local(), Data: data})
}

func (w *timedCacheWriter) WriteChunk(chunk TimedChunk) (int, error) {
	data := chunk.Data
	offset, err := w.file.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	n, err := w.file.Write(data)
	if err != nil {
		return n, err
	}
	if n > 0 {
		at := chunk.At
		if at.IsZero() {
			at = time.Now().Local()
		}
		entry := cacheIndexEntry{
			At:     at,
			Offset: offset,
			Length: int64(n),
		}
		if err := json.NewEncoder(w.index).Encode(entry); err != nil {
			return n, err
		}
	}
	return n, nil
}

func WriteTimedChunks(w io.Writer, chunks []TimedChunk) (int64, error) {
	var written int64
	for _, chunk := range chunks {
		if len(chunk.Data) == 0 {
			continue
		}
		var n int
		var err error
		if timed, ok := w.(*timedCacheWriter); ok {
			n, err = timed.WriteChunk(chunk)
		} else {
			n, err = w.Write(chunk.Data)
		}
		written += int64(n)
		if err != nil {
			return written, err
		}
		if n != len(chunk.Data) {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func ReadTimedChunks(cachePath string, indexPath string, start int64, data []byte) []TimedChunk {
	if len(data) == 0 {
		return nil
	}
	entries, err := readCacheIndex(indexPath)
	if err != nil || len(entries) == 0 {
		return []TimedChunk{{At: time.Now().Local(), Data: data}}
	}
	end := start + int64(len(data))
	var chunks []TimedChunk
	var coveredUntil int64 = start
	for _, entry := range entries {
		entryStart := entry.Offset
		entryEnd := entry.Offset + entry.Length
		if entryEnd <= start || entryStart >= end {
			continue
		}
		if coveredUntil < entryStart {
			chunks = append(chunks, TimedChunk{At: entry.At, Data: data[coveredUntil-start : entryStart-start]})
		}
		overlapStart := maxInt64(start, entryStart)
		overlapEnd := minInt64(end, entryEnd)
		if overlapEnd > overlapStart {
			chunks = append(chunks, TimedChunk{At: entry.At, Data: data[overlapStart-start : overlapEnd-start]})
			coveredUntil = overlapEnd
		}
	}
	if coveredUntil < end {
		at := time.Now().Local()
		if len(chunks) > 0 {
			at = chunks[len(chunks)-1].At
		}
		chunks = append(chunks, TimedChunk{At: at, Data: data[coveredUntil-start:]})
	}
	if len(chunks) == 0 {
		return []TimedChunk{{At: time.Now().Local(), Data: data}}
	}
	return chunks
}

func readCacheIndex(path string) ([]cacheIndexEntry, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var entries []cacheIndexEntry
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
	return entries, scanner.Err()
}

func CacheIndexPath(cachePath string) string {
	if cachePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cachePath), "cache.index.jsonl")
}

func LastLineChunks(chunks []TimedChunk, maxLines int) []TimedChunk {
	if maxLines <= 0 {
		return chunks
	}
	data := joinChunks(chunks)
	start := lastLinesStart(data, maxLines)
	if start == 0 {
		return chunks
	}
	return sliceChunks(chunks, int64(start), int64(len(data)))
}

func FormatTextChunks(chunks []TimedChunk, showTimestamps bool) []byte {
	if !showTimestamps {
		return joinChunks(chunks)
	}
	var out bytes.Buffer
	for _, chunk := range chunks {
		writeTimestampedTextChunk(&out, chunk)
	}
	return out.Bytes()
}

func FormatHexChunks(chunks []TimedChunk, showTimestamps bool) []byte {
	var out bytes.Buffer
	for _, chunk := range chunks {
		if len(chunk.Data) == 0 {
			continue
		}
		if showTimestamps {
			out.WriteString(formatTimestamp(chunk.At))
			out.WriteByte(' ')
		}
		out.WriteString(strings.TrimSuffix(FormatHexBytes(chunk.Data), "\n"))
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func writeTimestampedTextChunk(out *bytes.Buffer, chunk TimedChunk) {
	for len(chunk.Data) > 0 {
		line := chunk.Data
		if idx := bytes.IndexByte(chunk.Data, '\n'); idx >= 0 {
			line = chunk.Data[:idx+1]
			chunk.Data = chunk.Data[idx+1:]
		} else {
			chunk.Data = nil
		}
		out.WriteString(formatTimestamp(chunk.At))
		out.WriteByte(' ')
		out.Write(line)
	}
}

func formatTimestamp(at time.Time) string {
	if at.IsZero() {
		at = time.Now().Local()
	}
	return at.Local().Format("06-01-02 15:04:05.000")
}

func joinChunks(chunks []TimedChunk) []byte {
	var out bytes.Buffer
	for _, chunk := range chunks {
		out.Write(chunk.Data)
	}
	return out.Bytes()
}

func lastLinesStart(data []byte, maxLines int) int {
	if maxLines <= 0 {
		return 0
	}
	lines := 0
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] != '\n' {
			continue
		}
		lines++
		if lines > maxLines {
			return i + 1
		}
	}
	return 0
}

func sliceChunks(chunks []TimedChunk, start int64, end int64) []TimedChunk {
	var sliced []TimedChunk
	var pos int64
	for _, chunk := range chunks {
		chunkStart := pos
		chunkEnd := pos + int64(len(chunk.Data))
		pos = chunkEnd
		if chunkEnd <= start || chunkStart >= end {
			continue
		}
		overlapStart := maxInt64(start, chunkStart)
		overlapEnd := minInt64(end, chunkEnd)
		if overlapEnd > overlapStart {
			sliced = append(sliced, TimedChunk{
				At:   chunk.At,
				Data: chunk.Data[overlapStart-chunkStart : overlapEnd-chunkStart],
			})
		}
	}
	return sliced
}

func minInt64(a int64, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a int64, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
