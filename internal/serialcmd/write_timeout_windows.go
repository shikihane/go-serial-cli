//go:build windows

package serialcmd

import (
	"fmt"
	"reflect"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func writeSerialPortWithTimeout(port SerialPort, data []byte, timeout time.Duration) error {
	handle, ok := serialPortHandle(port)
	if !ok {
		return writeSerialPort(port, data)
	}
	var original windows.CommTimeouts
	if err := windows.GetCommTimeouts(handle, &original); err != nil {
		return writeSerialPort(port, data)
	}
	temporary := original
	ms := timeout.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	if ms > int64(^uint32(0)) {
		ms = int64(^uint32(0))
	}
	temporary.WriteTotalTimeoutConstant = uint32(ms)
	temporary.WriteTotalTimeoutMultiplier = 0
	if err := windows.SetCommTimeouts(handle, &temporary); err != nil {
		return writeSerialPort(port, data)
	}
	err := writeSerialPort(port, data)
	restoreErr := windows.SetCommTimeouts(handle, &original)
	if err != nil {
		return err
	}
	if restoreErr != nil {
		return fmt.Errorf("restore serial write timeout: %w", restoreErr)
	}
	return nil
}

func serialPortHandle(port SerialPort) (windows.Handle, bool) {
	value := reflect.ValueOf(port)
	if value.Kind() != reflect.Ptr || value.IsNil() {
		return 0, false
	}
	elem := value.Elem()
	if elem.Kind() != reflect.Struct {
		return 0, false
	}
	field := elem.FieldByName("handle")
	if !field.IsValid() || field.Type() != reflect.TypeOf(windows.Handle(0)) {
		return 0, false
	}
	return *(*windows.Handle)(unsafe.Pointer(field.UnsafeAddr())), true
}

func writeSerialPort(port SerialPort, data []byte) error {
	n, err := port.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return fmt.Errorf("short write %d/%d", n, len(data))
	}
	return nil
}
