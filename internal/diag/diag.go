package diag

import "fmt"

func MissingSetupCError() error {
	return fmt.Errorf("com0com setupc.exe not found\n\ndownload and install com0com first:\n  https://com0com.com/\n  https://sourceforge.net/projects/com0com/\n\nAfter install, make sure setupc.exe is on PATH or installed under Program Files.")
}

func MissingHub4comError() error {
	return fmt.Errorf("hub4com.exe not found\n\ndownload and install hub4com from the com0com project, then make hub4com.exe discoverable on PATH:\n  https://sourceforge.net/projects/com0com/")
}

func Hub4comStartError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("start hub4com: %w", err)
}

func SerialOpenError(port string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("open serial port %s: %w", port, err)
}
