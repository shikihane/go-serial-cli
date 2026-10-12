//go:build !unix && !windows

package cli

import "io"

// terminalWidth reports no terminal: the shell shows plain progress.
func terminalWidth(out io.Writer) int {
	return 0
}
