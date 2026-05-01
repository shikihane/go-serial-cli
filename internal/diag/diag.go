package diag

import (
	"errors"
	"fmt"
	"syscall"
)

func MissingSetupCError() error {
	return fmt.Errorf("com0com setupc.exe not found\n\ndownload and install com0com first:\n  https://com0com.com/\n  https://sourceforge.net/projects/com0com/\n\nAfter install, make sure setupc.exe is on PATH or installed under Program Files.")
}

func SerialOpenError(port string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.Errno(1)) {
		return fmt.Errorf("open serial port %s: %w\n\nWindows driver rejected the serial open/configuration request with ERROR_INVALID_FUNCTION. This usually means the USB-serial driver or device stack cannot open the port right now. Check `mode %s`, unplug/replug the adapter, or restart the device in Device Manager", port, err, port)
	}
	return fmt.Errorf("open serial port %s: %w", port, err)
}
