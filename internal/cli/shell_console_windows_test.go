//go:build windows

package cli

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestShellConsoleModeDisablesProcessedLineAndEchoInput(t *testing.T) {
	original := uint32(windows.ENABLE_PROCESSED_INPUT |
		windows.ENABLE_LINE_INPUT |
		windows.ENABLE_ECHO_INPUT |
		windows.ENABLE_MOUSE_INPUT |
		windows.ENABLE_EXTENDED_FLAGS)

	got := shellConsoleMode(original)

	for _, flag := range []uint32{
		windows.ENABLE_PROCESSED_INPUT,
		windows.ENABLE_LINE_INPUT,
		windows.ENABLE_ECHO_INPUT,
	} {
		if got&flag != 0 {
			t.Fatalf("shellConsoleMode(%#x) = %#x, still has disabled flag %#x", original, got, flag)
		}
	}
	for _, flag := range []uint32{
		windows.ENABLE_MOUSE_INPUT,
		windows.ENABLE_EXTENDED_FLAGS,
	} {
		if got&flag == 0 {
			t.Fatalf("shellConsoleMode(%#x) = %#x, lost preserved flag %#x", original, got, flag)
		}
	}
}
