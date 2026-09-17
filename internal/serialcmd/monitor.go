package serialcmd

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

type MonitorChunk struct {
	TimedChunk
	Direction string
}

func MergeMonitorChunks(rx []TimedChunk, tx []TimedChunk) []MonitorChunk {
	merged := make([]MonitorChunk, 0, len(rx)+len(tx))
	for _, chunk := range tx {
		merged = append(merged, MonitorChunk{TimedChunk: chunk, Direction: "TX"})
	}
	for _, chunk := range rx {
		merged = append(merged, MonitorChunk{TimedChunk: chunk, Direction: "RX"})
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].At.Before(merged[j].At)
	})
	return merged
}

func FormatMonitorChunks(chunks []MonitorChunk, outputHex bool) []byte {
	var out bytes.Buffer
	for _, chunk := range chunks {
		if len(chunk.Data) == 0 {
			continue
		}
		if outputHex {
			out.WriteString(formatTimestamp(chunk.At))
			out.WriteByte(' ')
			out.WriteString(monitorTag(chunk))
			out.WriteByte(' ')
			out.WriteString(strings.TrimSuffix(FormatHexBytes(chunk.Data), "\n"))
			out.WriteByte('\n')
			continue
		}
		writeMonitorTextChunk(&out, chunk)
	}
	return out.Bytes()
}

func writeMonitorTextChunk(out *bytes.Buffer, chunk MonitorChunk) {
	data := chunk.Data
	for len(data) > 0 {
		line := data
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			line = data[:idx]
			data = data[idx+1:]
		} else {
			data = nil
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		out.WriteString(formatTimestamp(chunk.At))
		out.WriteByte(' ')
		out.WriteString(monitorTag(chunk))
		out.WriteByte(' ')
		out.WriteString(escapeDisplayLine(line, 0))
		out.WriteByte('\n')
	}
}

func monitorTag(chunk MonitorChunk) string {
	if chunk.Direction != "TX" {
		return "RX"
	}
	if chunk.Source == "" {
		return "TX"
	}
	return "TX[" + chunk.Source + "]"
}

// escapeDisplayLine renders one captured line for terminal display: printable
// ASCII and tabs stay raw, everything else becomes \xNN so third-party binary
// payloads cannot corrupt the terminal. maxBytes > 0 truncates long payloads
// with a byte-count suffix; the on-disk capture always keeps the full data.
func escapeDisplayLine(data []byte, maxBytes int) string {
	truncated := false
	total := len(data)
	if maxBytes > 0 && total > maxBytes {
		data = data[:maxBytes]
		truncated = true
	}
	var b strings.Builder
	for _, ch := range data {
		if ch == '\t' || (ch >= 0x20 && ch <= 0x7e) {
			b.WriteByte(ch)
			continue
		}
		fmt.Fprintf(&b, "\\x%02x", ch)
	}
	if truncated {
		fmt.Fprintf(&b, " … (%d bytes)", total)
	}
	return b.String()
}

func TailLines(data []byte, maxLines int) []byte {
	return data[lastLinesStart(data, maxLines):]
}
