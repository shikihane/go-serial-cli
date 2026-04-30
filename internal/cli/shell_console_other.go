//go:build !windows

package cli

func configureShellConsole() func() {
	return func() {}
}

func isPlatformShellInputInterrupted(err error) bool {
	return false
}
