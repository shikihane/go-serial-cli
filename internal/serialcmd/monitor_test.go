package serialcmd

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestMergeMonitorChunksOrdersByTimestamp(t *testing.T) {
	t1 := time.Date(2026, 9, 17, 10, 0, 1, 0, time.Local)
	t2 := t1.Add(time.Second)
	rx := []TimedChunk{{At: t2, Data: []byte("VER 1.2\r\n")}}
	tx := []TimedChunk{{At: t1, Source: "COM20", Data: []byte("AT+VER\r\n")}}

	merged := MergeMonitorChunks(rx, tx)

	if len(merged) != 2 {
		t.Fatalf("merged chunks = %d, want 2", len(merged))
	}
	if merged[0].Direction != "TX" || merged[0].Source != "COM20" {
		t.Fatalf("merged[0] = %#v, want TX from COM20", merged[0])
	}
	if merged[1].Direction != "RX" || merged[1].Source != "" {
		t.Fatalf("merged[1] = %#v, want RX", merged[1])
	}
}

func TestFormatMonitorChunksTextTagsDirectionAndSource(t *testing.T) {
	t1 := time.Date(2026, 9, 17, 10, 0, 1, 0, time.Local)
	merged := MergeMonitorChunks(
		[]TimedChunk{{At: t1.Add(time.Second), Data: []byte("VER 1.2\r\n")}},
		[]TimedChunk{{At: t1, Source: "COM20", Data: []byte("AT+VER\r\n")}},
	)

	out := string(FormatMonitorChunks(merged, false))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %#v, want 2 lines", lines)
	}
	if !strings.Contains(lines[0], "TX[COM20] AT+VER") {
		t.Fatalf("line[0] = %q, want TX[COM20] AT+VER", lines[0])
	}
	if !strings.Contains(lines[1], "RX VER 1.2") {
		t.Fatalf("line[1] = %q, want RX VER 1.2", lines[1])
	}
	if !strings.HasPrefix(lines[0], "26-09-17 ") {
		t.Fatalf("line[0] = %q, want leading timestamp", lines[0])
	}
}

func TestFormatMonitorChunksSplitsMultiLineData(t *testing.T) {
	t1 := time.Date(2026, 9, 17, 10, 0, 1, 0, time.Local)
	merged := MergeMonitorChunks([]TimedChunk{{At: t1, Data: []byte("a\nb")}}, nil)

	out := string(FormatMonitorChunks(merged, false))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %#v, want 2 lines", lines)
	}
	if !strings.HasSuffix(lines[0], "RX a") || !strings.HasSuffix(lines[1], "RX b") {
		t.Fatalf("lines = %#v, want each line prefixed with RX", lines)
	}
}

func TestFormatMonitorChunksHex(t *testing.T) {
	t1 := time.Date(2026, 9, 17, 10, 0, 1, 0, time.Local)
	merged := MergeMonitorChunks(
		[]TimedChunk{{At: t1.Add(time.Second), Data: []byte{0x01, 0x02}}},
		[]TimedChunk{{At: t1, Source: "COM20", Data: []byte{0xAA}}},
	)

	out := string(FormatMonitorChunks(merged, true))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %#v, want 2 lines", lines)
	}
	if !strings.HasSuffix(lines[0], "TX[COM20] aa") {
		t.Fatalf("line[0] = %q, want TX[COM20] aa", lines[0])
	}
	if !strings.HasSuffix(lines[1], "RX 01 02") {
		t.Fatalf("line[1] = %q, want RX 01 02", lines[1])
	}
}

func TestFormatMonitorChunksEscapesNonPrintableBytes(t *testing.T) {
	t1 := time.Date(2026, 9, 17, 10, 0, 1, 0, time.Local)
	merged := MergeMonitorChunks(nil, []TimedChunk{{At: t1, Source: "COM20", Data: []byte{0x01, 'A', 0x1b, '\r', '\n'}}})

	out := string(FormatMonitorChunks(merged, false))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %#v, want 1 line", lines)
	}
	if !strings.HasSuffix(lines[0], `TX[COM20] \x01A\x1b`) {
		t.Fatalf("line = %q, want escaped payload with CR stripped", lines[0])
	}
}

func TestEscapeDisplayLineTruncatesLongPayload(t *testing.T) {
	data := bytes.Repeat([]byte{'A'}, 100)
	got := escapeDisplayLine(data, 64)
	if !strings.HasPrefix(got, strings.Repeat("A", 64)) {
		t.Fatalf("escaped = %q, want 64 leading A bytes", got)
	}
	if !strings.HasSuffix(got, "(100 bytes)") {
		t.Fatalf("escaped = %q, want byte-count suffix", got)
	}
}

func TestTailLinesReturnsLastLines(t *testing.T) {
	if got := string(TailLines([]byte("a\nb\nc\n"), 2)); got != "b\nc\n" {
		t.Fatalf("TailLines = %q, want last two lines", got)
	}
	if got := string(TailLines([]byte("a\nb\n"), 0)); got != "a\nb\n" {
		t.Fatalf("TailLines with 0 = %q, want all data", got)
	}
}

func TestTimedCacheWriterRoundTripsSource(t *testing.T) {
	dir := t.TempDir()
	cachePath := dir + "/tx.log"
	indexPath := dir + "/tx.index.jsonl"
	writer, closeWriter, err := OpenTimedCacheWriter(cachePath, indexPath)
	if err != nil {
		t.Fatalf("OpenTimedCacheWriter returned error: %v", err)
	}
	at := time.Date(2026, 9, 17, 10, 0, 1, 0, time.Local)
	if _, err := WriteTimedChunks(writer, []TimedChunk{{At: at, Source: "COM20", Data: []byte("AT\r\n")}}); err != nil {
		t.Fatalf("WriteTimedChunks returned error: %v", err)
	}
	closeWriter()

	chunks := ReadTimedChunks(cachePath, indexPath, 0, []byte("AT\r\n"))
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	if chunks[0].Source != "COM20" {
		t.Fatalf("Source = %q, want COM20", chunks[0].Source)
	}
	if !chunks[0].At.Equal(at) {
		t.Fatalf("At = %v, want %v", chunks[0].At, at)
	}
}
