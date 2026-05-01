package diag_test

import (
	"errors"
	"strings"
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
