package serialcmd

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

func ParseTextPayload(value string) ([]byte, error) {
	return parsePayload(value)
}

func ParseHexPayload(tokens []string) ([]byte, error) {
	var digits strings.Builder
	for _, token := range tokens {
		for i := 0; i < len(token); i++ {
			ch := token[i]
			switch {
			case ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' || ch == ',' || ch == '-':
				continue
			case ch == '0' && i+1 < len(token) && (token[i+1] == 'x' || token[i+1] == 'X'):
				i++
				continue
			case isHexDigit(ch):
				digits.WriteByte(ch)
			default:
				return nil, fmt.Errorf("invalid hex payload at %q", token)
			}
		}
	}
	text := digits.String()
	if text == "" {
		return nil, errors.New("hex payload is empty")
	}
	if len(text)%2 != 0 {
		return nil, errors.New("hex payload must contain an even number of digits")
	}
	out, err := hex.DecodeString(text)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("hex payload is empty")
	}
	return out, nil
}

func ReadRawPayloadFile(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("payload file is required")
	}
	return os.ReadFile(path)
}

func ReadHexPayloadFile(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("hex payload file is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseHexPayload([]string{string(data)})
}

func FormatHexBytes(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	var b strings.Builder
	for i, ch := range data {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(hex.EncodeToString([]byte{ch}))
	}
	b.WriteByte('\n')
	return b.String()
}

func isHexDigit(ch byte) bool {
	return (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
}

func inputLinePayload(line string) ([]byte, error) {
	payload, err := parsePayload(line)
	if err != nil {
		return nil, err
	}
	if strings.Contains(line, `\r`) || strings.Contains(line, `\n`) || hasLineEnding(payload) {
		return payload, nil
	}
	return append(payload, '\r', '\n'), nil
}

func hasLineEnding(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	last := value[len(value)-1]
	return last == '\r' || last == '\n'
}

func unescape(value string) string {
	payload, err := parsePayload(value)
	if err != nil {
		return value
	}
	return string(payload)
}

func parsePayload(value string) ([]byte, error) {
	payload := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			payload = append(payload, value[i])
			continue
		}
		if i+1 >= len(value) {
			payload = append(payload, value[i])
			continue
		}

		i++
		switch value[i] {
		case 'r':
			payload = append(payload, '\r')
		case 'n':
			payload = append(payload, '\n')
		case 't':
			payload = append(payload, '\t')
		case 'x':
			if i+2 >= len(value) {
				return nil, fmt.Errorf("invalid hex escape at byte %d: want \\xNN", i-1)
			}
			hi, ok := hexValue(value[i+1])
			if !ok {
				return nil, fmt.Errorf("invalid hex escape at byte %d: want \\xNN", i-1)
			}
			lo, ok := hexValue(value[i+2])
			if !ok {
				return nil, fmt.Errorf("invalid hex escape at byte %d: want \\xNN", i-1)
			}
			payload = append(payload, hi<<4|lo)
			i += 2
		case 'c':
			if i+1 >= len(value) {
				return nil, fmt.Errorf("invalid control escape at byte %d: want \\cA through \\cZ", i-1)
			}
			ch := value[i+1]
			if ch >= 'a' && ch <= 'z' {
				ch -= 'a' - 'A'
			}
			if ch < 'A' || ch > 'Z' {
				return nil, fmt.Errorf("invalid control escape at byte %d: want \\cA through \\cZ", i-1)
			}
			payload = append(payload, ch-'A'+1)
			i++
		default:
			payload = append(payload, '\\', value[i])
		}
	}
	return payload, nil
}

func hexValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}
