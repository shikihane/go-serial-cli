//go:build unix

package cli

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// terminalWidth returns the columns of the terminal out writes to, or 0
// when out is no terminal.
func terminalWidth(out io.Writer) int {
	f, ok := out.(*os.File)
	if !ok {
		return 0
	}
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(ws.Col)
}

// terminalRows returns the visible rows of the terminal out writes to, or
// 0 when out is no terminal. The cursor row is unknown without asking the
// terminal, so the clear sequence starts at the last row instead.
func terminalRows(out io.Writer) (int, int) {
	f, ok := out.(*os.File)
	if !ok {
		return 0, 0
	}
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return int(ws.Row), -1
}
