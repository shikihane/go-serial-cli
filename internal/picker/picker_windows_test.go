//go:build windows

package picker

import (
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The dialogs themselves need a person; these tests cover only the parts
// that do not open a window.

func TestSplitMultiSelectSingleFile(t *testing.T) {
	buf := windows.StringToUTF16(`C:\data\a.bin`)
	buf = append(buf, 0)
	got := splitMultiSelect(buf)
	if len(got) != 1 || got[0] != `C:\data\a.bin` {
		t.Fatalf("got %q", got)
	}
}

func TestSplitMultiSelectSeveralFiles(t *testing.T) {
	var buf []uint16
	for _, part := range []string{`C:\data`, "a.bin", "b.wav"} {
		buf = append(buf, windows.StringToUTF16(part)...)
	}
	buf = append(buf, 0, 0)
	got := splitMultiSelect(buf)
	want := []string{filepath.Join(`C:\data`, "a.bin"), filepath.Join(`C:\data`, "b.wav")}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestOpenFileNameMatchesWin64Layout(t *testing.T) {
	// sizeof(OPENFILENAMEW) is 152 on 64-bit Windows and 88 on 32-bit.
	want := uintptr(152)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		want = 88
	}
	if got := unsafe.Sizeof(openFileName{}); got != want {
		t.Fatalf("sizeof(openFileName) = %d, want %d", got, want)
	}
}

func TestDialogUnavailableOverSSH(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "10.0.0.1 22 10.0.0.2 50000")
	if available() {
		t.Fatal("dialogs must be unavailable in an SSH session")
	}
}
