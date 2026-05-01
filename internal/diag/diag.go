package diag

import "fmt"

func MissingSetupCError() error {
	return fmt.Errorf("com0com setupc.exe not found\n\ndownload and install com0com first:\n  https://com0com.com/\n  https://sourceforge.net/projects/com0com/\n\nAfter install, make sure setupc.exe is on PATH or installed under Program Files.")
}

func SerialOpenError(port string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("open serial port %s: %w", port, err)
}
