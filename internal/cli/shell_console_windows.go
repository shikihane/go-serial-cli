//go:build windows

package cli

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureShellConsole() func() {
	handle := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return func() {}
	}
	if err := windows.SetConsoleMode(handle, shellConsoleMode(mode)); err != nil {
		return func() {}
	}
	return func() {
		_ = windows.SetConsoleMode(handle, mode)
	}
}

func shellConsoleMode(mode uint32) uint32 {
	return mode &^ (windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_ECHO_INPUT)
}

func isPlatformShellInputInterrupted(err error) bool {
	return errors.Is(err, syscall.ERROR_OPERATION_ABORTED)
}
