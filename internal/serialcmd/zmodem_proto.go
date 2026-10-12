package serialcmd

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// ZMODEM framing as defined by Chuck Forsberg's 1988 specification. Names
// follow the spec and lrzsz so the code can be read side by side with them.
const (
	zPAD   byte = '*'
	zDLE   byte = 0x18
	zDLEE  byte = zDLE ^ 0x40
	zBIN   byte = 'A'
	zHEX   byte = 'B'
	zBIN32 byte = 'C'

	asciiCAN  byte = 0x18
	asciiBS   byte = 0x08
	asciiDLE  byte = 0x10
	asciiXON  byte = 0x11
	asciiXOFF byte = 0x13
)

// Frame types.
const (
	zRQINIT    byte = 0
	zRINIT     byte = 1
	zSINIT     byte = 2
	zACK       byte = 3
	zFILE      byte = 4
	zSKIP      byte = 5
	zNAK       byte = 6
	zABORT     byte = 7
	zFIN       byte = 8
	zRPOS      byte = 9
	zDATA      byte = 10
	zEOF       byte = 11
	zFERR      byte = 12
	zCRC       byte = 13
	zCHALLENGE byte = 14
	zCOMPL     byte = 15
	zCAN       byte = 16
	zFREECNT   byte = 17
	zCOMMAND   byte = 18
	zSTDERR    byte = 19
)

// Data subpacket frame ends and the escaped rubout bytes.
const (
	zCRCE byte = 'h'
	zCRCG byte = 'i'
	zCRCQ byte = 'j'
	zCRCW byte = 'k'
	zRUB0 byte = 'l'
	zRUB1 byte = 'm'
)

// ZRINIT capability flags in ZF0.
const (
	zCanFDX  byte = 0x01
	zCanOVIO byte = 0x02
	zCanBRK  byte = 0x04
	zCanFC32 byte = 0x20
	zEscCtl  byte = 0x40
	zEsc8    byte = 0x80
)

// ZSINIT flags in ZF0.
const (
	zTEscCtl byte = 0x40
)

// ZFILE conversion options in ZF0.
const (
	zCBIN   byte = 1
	zCNL    byte = 2
	zCRESUM byte = 3
)

// ZFILE management options in ZF1. zMSkNoLoc is a flag; the rest, under
// zMMask, are one choice.
const (
	zMSkNoLoc byte = 0x80
	zMMask    byte = 0x1f
	zMNEWL    byte = 1
	zMCRC     byte = 2
	zMAPND    byte = 3
	zMCLOB    byte = 4
	zMNEW     byte = 5
	zMDIFF    byte = 6
	zMPROT    byte = 7
	zMCHNG    byte = 8
)

// zCancelSequence aborts a transfer: eight CAN to reach the five the peer
// needs, then backspaces to erase them from a terminal line.
var zCancelSequence = []byte{
	asciiCAN, asciiCAN, asciiCAN, asciiCAN, asciiCAN, asciiCAN, asciiCAN, asciiCAN,
	asciiBS, asciiBS, asciiBS, asciiBS, asciiBS, asciiBS, asciiBS, asciiBS,
}

// zHeader holds a frame type and its four header bytes. Data[0..3] are
// ZP0..ZP3 for positions and ZF3..ZF0 for flags, as in the spec.
type zHeader struct {
	Type   byte
	Data   [4]byte
	Format byte
}

func zPosHeader(frameType byte, pos uint32) zHeader {
	h := zHeader{Type: frameType}
	binary.LittleEndian.PutUint32(h.Data[:], pos)
	return h
}

func zFlagsHeader(frameType byte, f0, f1, f2, f3 byte) zHeader {
	return zHeader{Type: frameType, Data: [4]byte{f3, f2, f1, f0}}
}

func (h zHeader) pos() uint32 { return binary.LittleEndian.Uint32(h.Data[:]) }
func (h zHeader) f0() byte    { return h.Data[3] }
func (h zHeader) f1() byte    { return h.Data[2] }

func zFrameName(t byte) string {
	names := []string{"ZRQINIT", "ZRINIT", "ZSINIT", "ZACK", "ZFILE", "ZSKIP", "ZNAK",
		"ZABORT", "ZFIN", "ZRPOS", "ZDATA", "ZEOF", "ZFERR", "ZCRC", "ZCHALLENGE",
		"ZCOMPL", "ZCAN", "ZFREECNT", "ZCOMMAND", "ZSTDERR"}
	if int(t) < len(names) {
		return names[t]
	}
	return fmt.Sprintf("frame %d", t)
}

// crc16XModem is CRC-16/XMODEM: poly 0x1021, init 0, no reflection.
func crc16XModem(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc = crc16Update(crc, b)
	}
	return crc
}

func crc16Update(crc uint16, b byte) uint16 {
	crc ^= uint16(b) << 8
	for i := 0; i < 8; i++ {
		if crc&0x8000 != 0 {
			crc = (crc << 1) ^ 0x1021
		} else {
			crc <<= 1
		}
	}
	return crc
}

const zHexDigits = "0123456789abcdef"

// encodeHexHeader builds a ZHEX header. Hex headers end with CR, LF|0x80
// and, except for ZACK and ZFIN, an XON, as lrzsz sends them.
func encodeHexHeader(h zHeader) []byte {
	raw := []byte{h.Type, h.Data[0], h.Data[1], h.Data[2], h.Data[3]}
	crc := crc16XModem(raw)
	raw = append(raw, byte(crc>>8), byte(crc))
	out := make([]byte, 0, 22)
	out = append(out, zPAD, zPAD, zDLE, zHEX)
	for _, b := range raw {
		out = append(out, zHexDigits[b>>4], zHexDigits[b&0x0f])
	}
	out = append(out, '\r', 0x8a)
	if h.Type != zACK && h.Type != zFIN {
		out = append(out, asciiXON)
	}
	return out
}

// zEscaper applies ZDLE escaping. It always escapes ZDLE, DLE, XON, XOFF
// and their high-bit forms, and a CR that follows '@' (telenet escape). With
// ctl set it escapes every control character.
type zEscaper struct {
	ctl  bool
	last byte
}

func (e *zEscaper) needs(b byte) bool {
	switch b {
	case zDLE, asciiDLE, asciiXON, asciiXOFF, zDLE | 0x80, asciiDLE | 0x80, asciiXON | 0x80, asciiXOFF | 0x80:
		return true
	case '\r', '\r' | 0x80:
		return e.ctl || e.last&0x7f == '@'
	}
	return e.ctl && b&0x60 == 0
}

func (e *zEscaper) put(dst []byte, b byte) []byte {
	if e.needs(b) {
		dst = append(dst, zDLE, b^0x40)
	} else {
		dst = append(dst, b)
	}
	e.last = b
	return dst
}

func (e *zEscaper) putAll(dst []byte, data []byte) []byte {
	for _, b := range data {
		dst = e.put(dst, b)
	}
	return dst
}

// encodeBinHeader builds a ZBIN or ZBIN32 header.
func encodeBinHeader(h zHeader, crc32Mode bool, esc *zEscaper) []byte {
	raw := []byte{h.Type, h.Data[0], h.Data[1], h.Data[2], h.Data[3]}
	out := make([]byte, 0, 24)
	out = append(out, zPAD, zDLE)
	if crc32Mode {
		out = append(out, zBIN32)
		crc := crc32.ChecksumIEEE(raw)
		var c [4]byte
		binary.LittleEndian.PutUint32(c[:], crc)
		raw = append(raw, c[:]...)
	} else {
		out = append(out, zBIN)
		crc := crc16XModem(raw)
		raw = append(raw, byte(crc>>8), byte(crc))
	}
	esc.last = 0
	return esc.putAll(out, raw)
}

// encodeSubpacket escapes data and appends ZDLE, the frame end and the CRC.
// A ZCRCW subpacket is followed by XON, as lrzsz sends it.
func encodeSubpacket(dst []byte, data []byte, end byte, crc32Mode bool, esc *zEscaper) []byte {
	dst = esc.putAll(dst, data)
	dst = append(dst, zDLE, end)
	esc.last = end
	if crc32Mode {
		crc := crc32.Update(crc32.ChecksumIEEE(data), crc32.IEEETable, []byte{end})
		var c [4]byte
		binary.LittleEndian.PutUint32(c[:], crc)
		dst = esc.putAll(dst, c[:])
	} else {
		crc := crc16Update(crc16XModem(data), end)
		dst = esc.putAll(dst, []byte{byte(crc >> 8), byte(crc)})
	}
	if end == zCRCW {
		dst = append(dst, asciiXON)
	}
	return dst
}

func zHexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// decodeHexHeaderDigits checks 14 hex digits (type, four bytes, CRC16) and
// returns the header when the CRC matches.
func decodeHexHeaderDigits(digits []byte) (zHeader, bool) {
	if len(digits) != 14 {
		return zHeader{}, false
	}
	var raw [7]byte
	for i := range raw {
		hi, ok1 := zHexValue(digits[2*i] & 0x7f)
		lo, ok2 := zHexValue(digits[2*i+1] & 0x7f)
		if !ok1 || !ok2 {
			return zHeader{}, false
		}
		raw[i] = hi<<4 | lo
	}
	crc := crc16XModem(raw[:5])
	if crc != uint16(raw[5])<<8|uint16(raw[6]) {
		return zHeader{}, false
	}
	return zHeader{Type: raw[0], Data: [4]byte{raw[1], raw[2], raw[3], raw[4]}, Format: zHEX}, true
}
