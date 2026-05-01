package diag_test

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"go-serial-cli/internal/diag"
)

func TestMissingSetupCErrorIncludesActionableHint(t *testing.T) {
	err := diag.MissingSetupCError()
	got := err.Error()
	for _, want := range []string{
		"com0com setupc.exe not found",
		"download and install com0com",
		"https://com0com.com/",
		"https://sourceforge.net/projects/com0com/",
		"setupc.exe is on PATH",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("error %q does not contain %q", got, want)
		}
	}
}

func TestSerialOpenErrorIncludesPortAndCause(t *testing.T) {
	err := diag.SerialOpenError("COM3", errors.New("access denied"))
	got := err.Error()
	for _, want := range []string{"open serial port COM3", "access denied"} {
		if !strings.Contains(got, want) {
			t.Fatalf("error %q does not contain %q", got, want)
		}
	}
}

func TestSerialOpenErrorExplainsWindowsIncorrectFunction(t *testing.T) {
	err := diag.SerialOpenError("COM5", syscall.Errno(1))
	got := err.Error()
	for _, want := range []string{
		"open serial port COM5",
		"Incorrect function",
		"Windows driver rejected the serial open/configuration request",
		"USB-serial driver",
		"mode COM5",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("error %q does not contain %q", got, want)
		}
	}
}
