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

	got := shellInputConsoleMode(original)

	for _, flag := range []uint32{
		windows.ENABLE_PROCESSED_INPUT,
		windows.ENABLE_LINE_INPUT,
		windows.ENABLE_ECHO_INPUT,
	} {
		if got&flag != 0 {
			t.Fatalf("shellInputConsoleMode(%#x) = %#x, still has disabled flag %#x", original, got, flag)
		}
	}
	for _, flag := range []uint32{
		windows.ENABLE_MOUSE_INPUT,
		windows.ENABLE_EXTENDED_FLAGS,
	} {
		if got&flag == 0 {
			t.Fatalf("shellInputConsoleMode(%#x) = %#x, lost preserved flag %#x", original, got, flag)
		}
	}
}

func TestShellOutputConsoleModeEnablesVirtualTerminalProcessing(t *testing.T) {
	original := uint32(windows.ENABLE_EXTENDED_FLAGS)

	got := shellOutputConsoleMode(original)

	if got&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
		t.Fatalf("shellOutputConsoleMode(%#x) = %#x, want virtual terminal processing enabled", original, got)
	}
	if got&windows.ENABLE_PROCESSED_OUTPUT == 0 {
		t.Fatalf("shellOutputConsoleMode(%#x) = %#x, want processed output enabled", original, got)
	}
	if got&windows.ENABLE_EXTENDED_FLAGS == 0 {
		t.Fatalf("shellOutputConsoleMode(%#x) = %#x, lost preserved flag", original, got)
	}
}

func TestShellConsoleKeyBytesMapsArrowKeysToEscapeSequences(t *testing.T) {
	tests := []struct {
		name string
		key  uint16
		want string
	}{
		{"up", vkUp, "\x1b[A"},
		{"down", vkDown, "\x1b[B"},
		{"right", vkRight, "\x1b[C"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellConsoleKeyBytes(keyEventRecord{VirtualKeyCode: tt.key})
			if string(got) != tt.want {
				t.Fatalf("shellConsoleKeyBytes(%#x) = %q, want %q", tt.key, string(got), tt.want)
			}
		})
	}
}

func TestShellConsoleKeyBytesReturnsPrintableUnicode(t *testing.T) {
	got := shellConsoleKeyBytes(keyEventRecord{UnicodeChar: 'A'})
	if string(got) != "A" {
		t.Fatalf("shellConsoleKeyBytes('A') = %q, want A", string(got))
	}
}
