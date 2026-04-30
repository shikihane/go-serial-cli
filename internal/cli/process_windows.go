//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	detachedProcess       uint32 = 0x00000008
	createNewProcessGroup uint32 = 0x00000200
	seeMaskNoCloseProcess uint32 = 0x00000040
)

var procShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         windows.Handle
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     windows.Handle
	lpIDList     unsafe.Pointer
	lpClass      *uint16
	hkeyClass    windows.Handle
	dwHotKey     uint32
	hIcon        windows.Handle
	hProcess     windows.Handle
}

func configureBackgroundProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= detachedProcess | createNewProcessGroup
	cmd.SysProcAttr.HideWindow = true
}

func isElevationRequired(err error) bool {
	return errors.Is(err, windows.ERROR_ELEVATION_REQUIRED)
}

func runSetupCOperationsElevatedInWindow(ops []setupCOperation) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	encoded, err := encodeSetupCOperations(ops)
	if err != nil {
		return err
	}
	params := windowsCommandLine([]string{"admin", "setupc-batch", encoded})
	return runElevatedAndWait(exe, params)
}

func runElevatedAndWait(file string, params string) error {
	verbPtr, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	filePtr, err := windows.UTF16PtrFromString(file)
	if err != nil {
		return err
	}
	paramsPtr, err := windows.UTF16PtrFromString(params)
	if err != nil {
		return err
	}
	info := shellExecuteInfo{
		cbSize:       uint32(unsafe.Sizeof(shellExecuteInfo{})),
		fMask:        seeMaskNoCloseProcess,
		lpVerb:       verbPtr,
		lpFile:       filePtr,
		lpParameters: paramsPtr,
		nShow:        windows.SW_SHOWNORMAL,
	}
	r1, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if r1 == 0 {
		if callErr != windows.ERROR_SUCCESS {
			return callErr
		}
		return windows.GetLastError()
	}
	defer windows.CloseHandle(info.hProcess)

	event, err := windows.WaitForSingleObject(info.hProcess, windows.INFINITE)
	if err != nil {
		return err
	}
	if event != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("wait for elevated setupc: unexpected wait result %#x", event)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(info.hProcess, &exitCode); err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("elevated setupc exited with code %d", exitCode)
	}
	return nil
}

func windowsCommandLine(args []string) string {
	escaped := make([]string, 0, len(args))
	for _, arg := range args {
		escaped = append(escaped, syscall.EscapeArg(arg))
	}
	return strings.Join(escaped, " ")
}

func pauseAdminWindow() {
	cmd := exec.Command("cmd", "/c", "pause")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stdout, "Press Enter to exit...")
		_, _ = fmt.Fscanln(os.Stdin)
	}
}
