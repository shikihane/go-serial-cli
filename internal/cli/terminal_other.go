//go:build !unix && !windows

package cli

import "io"

// terminalWidth reports no terminal: the shell shows plain progress.
func terminalWidth(out io.Writer) int {
	return 0
}

// terminalRows reports no terminal, so the shell clear leaves the
// screen to the terminal itself.
func terminalRows(out io.Writer) (int, int) {
	return 0, 0
}
