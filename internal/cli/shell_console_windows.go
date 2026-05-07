//go:build windows

package cli

import (
	"errors"
	"io"
	"os"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

func configureShellConsole() func() {
	inputHandle := windows.Handle(os.Stdin.Fd())
	inputMode, inputOK := configureConsoleMode(inputHandle, shellInputConsoleMode)
	outputHandle := windows.Handle(os.Stdout.Fd())
	outputMode, outputOK := configureConsoleMode(outputHandle, shellOutputConsoleMode)
	return func() {
		if inputOK {
			_ = windows.SetConsoleMode(inputHandle, inputMode)
		}
		if outputOK {
			_ = windows.SetConsoleMode(outputHandle, outputMode)
		}
	}
}

func configureConsoleMode(handle windows.Handle, modeFunc func(uint32) uint32) (uint32, bool) {
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return 0, false
	}
	if err := windows.SetConsoleMode(handle, modeFunc(mode)); err != nil {
		return 0, false
	}
	return mode, true
}

func shellInputConsoleMode(mode uint32) uint32 {
	return mode &^ (windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_ECHO_INPUT)
}

func shellOutputConsoleMode(mode uint32) uint32 {
	return mode | windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
}

func shellConsoleInput(input io.Reader) io.Reader {
	if input != os.Stdin {
		return input
	}
	handle := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return input
	}
	return &shellWindowsConsoleReader{handle: handle, fallback: input}
}

func isPlatformShellInputInterrupted(err error) bool {
	return errors.Is(err, syscall.ERROR_OPERATION_ABORTED)
}

type shellWindowsConsoleReader struct {
	handle   windows.Handle
	fallback io.Reader
	pending  []byte
}

func (r *shellWindowsConsoleReader) Read(buf []byte) (int, error) {
	return r.read(buf)
}

func (r *shellWindowsConsoleReader) read(buf []byte) (int, error) {
	if len(r.pending) > 0 {
		n := copy(buf, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	for {
		var record inputRecord
		var read uint32
		err := readConsoleInput(r.handle, &record, 1, &read)
		if err != nil {
			if r.fallback != nil {
				return r.fallback.Read(buf)
			}
			return 0, err
		}
		if read == 0 || record.EventType != keyEvent || record.KeyEvent.KeyDown == 0 {
			continue
		}
		data := shellConsoleKeyBytes(record.KeyEvent)
		if len(data) == 0 {
			continue
		}
		n := copy(buf, data)
		if n < len(data) {
			r.pending = append(r.pending[:0], data[n:]...)
		}
		return n, nil
	}
}

func shellConsoleKeyBytes(event keyEventRecord) []byte {
	switch event.VirtualKeyCode {
	case vkUp:
		return []byte{0x1b, '[', 'A'}
	case vkDown:
		return []byte{0x1b, '[', 'B'}
	case vkRight:
		return []byte{0x1b, '[', 'C'}
	case vkLeft:
		return nil
	}
	if event.UnicodeChar == 0 {
		return nil
	}
	return []byte(string(utf16.Decode([]uint16{event.UnicodeChar})))
}

const (
	keyEvent = 0x0001
	vkLeft   = 0x25
	vkUp     = 0x26
	vkRight  = 0x27
	vkDown   = 0x28
)

type inputRecord struct {
	EventType uint16
	_         uint16
	KeyEvent  keyEventRecord
}

type keyEventRecord struct {
	KeyDown         int32
	RepeatCount     uint16
	VirtualKeyCode  uint16
	VirtualScanCode uint16
	UnicodeChar     uint16
	ControlKeyState uint32
}

var procReadConsoleInputW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleInputW")

func readConsoleInput(handle windows.Handle, record *inputRecord, length uint32, read *uint32) error {
	r1, _, err := procReadConsoleInputW.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(record)),
		uintptr(length),
		uintptr(unsafe.Pointer(read)),
	)
	if r1 == 0 {
		if err != syscall.Errno(0) {
			return err
		}
		return syscall.EINVAL
	}
	return nil
}
