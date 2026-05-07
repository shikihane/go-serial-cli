//go:build !windows

package cli

import "io"

func configureShellConsole() func() {
	return func() {}
}

func shellConsoleInput(input io.Reader) io.Reader {
	return input
}

func isPlatformShellInputInterrupted(err error) bool {
	return false
}
