//go:build windows

// Package picker shows the Windows common file dialogs for transfers that
// a device starts from sio shell.
package picker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"go-serial-cli/internal/serialcmd"

	"golang.org/x/sys/windows"
)

var (
	comdlg32 = windows.NewLazySystemDLL("comdlg32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")

	procGetOpenFileNameW         = comdlg32.NewProc("GetOpenFileNameW")
	procGetSaveFileNameW         = comdlg32.NewProc("GetSaveFileNameW")
	procCommDlgExtendedError     = comdlg32.NewProc("CommDlgExtendedError")
	procSHBrowseForFolderW       = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW     = shell32.NewProc("SHGetPathFromIDListW")
	procGetConsoleWindow         = kernel32.NewProc("GetConsoleWindow")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procSendMessageW             = user32.NewProc("SendMessageW")
	procGetProcessWindowStation  = user32.NewProc("GetProcessWindowStation")
	procGetUserObjectInformation = user32.NewProc("GetUserObjectInformationW")
	procCoUninitialize           = ole32.NewProc("CoUninitialize")
	procCoTaskMemFree            = ole32.NewProc("CoTaskMemFree")
	procEnumThreadWindows        = user32.NewProc("EnumThreadWindows")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procGetCurrentThreadID       = kernel32.NewProc("GetCurrentThreadId")
)

const (
	ofnOverwritePrompt  = 0x00000002
	ofnHideReadOnly     = 0x00000004
	ofnNoChangeDir      = 0x00000008
	ofnAllowMultiSelect = 0x00000200
	ofnPathMustExist    = 0x00000800
	ofnFileMustExist    = 0x00001000
	ofnExplorer         = 0x00080000

	bifReturnOnlyFSDirs = 0x0001
	bifEditBox          = 0x0010
	bifNewDialogStyle   = 0x0040
	bffmInitialized     = 1
	bffmSetSelectionW   = 0x0400 + 103

	uoiFlags   = 1
	wsfVisible = 1

	fileBufferChars = 64 * 1024

	wmClose = 0x0010
)

type openFileName struct {
	structSize    uint32
	owner         uintptr
	instance      uintptr
	filter        *uint16
	customFilter  *uint16
	maxCustFilter uint32
	filterIndex   uint32
	file          *uint16
	maxFile       uint32
	fileTitle     *uint16
	maxFileTitle  uint32
	initialDir    *uint16
	title         *uint16
	flags         uint32
	fileOffset    uint16
	fileExtension uint16
	defExt        *uint16
	custData      uintptr
	hook          uintptr
	templateName  *uint16
	reserved      uintptr
	reserved2     uint32
	flagsEx       uint32
}

type browseInfo struct {
	owner       uintptr
	root        uintptr
	displayName *uint16
	title       *uint16
	flags       uint32
	callback    uintptr
	param       uintptr
	image       int32
}

// browseCallback selects the start folder once the dialog is up. Windows
// allows only so many callbacks per process, so there is one.
var browseCallback = sync.OnceValue(func() uintptr {
	return windows.NewCallback(func(hwnd, msg, lparam, data uintptr) uintptr {
		if msg == bffmInitialized && data != 0 {
			_, _, _ = procSendMessageW.Call(hwnd, bffmSetSelectionW, 1, data)
		}
		return 0
	})
})

type userObjectFlags struct {
	inherit  int32
	reserved int32
	flags    uint32
}

// Dialog shows the common dialogs, owned by the terminal window.
type Dialog struct{}

// New returns the dialog picker.
func New() serialcmd.FilePicker { return Dialog{} }

// uiThread is the OS thread that runs the open dialog, 0 when none.
var uiThread atomic.Uint32

// closeWindow posts WM_CLOSE to each top-level window of the dialog
// thread: the common dialogs treat it as Cancel.
var closeWindow = sync.OnceValue(func() uintptr {
	return windows.NewCallback(func(hwnd, lparam uintptr) uintptr {
		_, _, _ = procPostMessageW.Call(hwnd, wmClose, 0, 0)
		return 1
	})
})

// Close cancels a dialog that is showing, as if the person pressed
// Cancel. It is used when the device gives up while the dialog is open.
func (Dialog) Close() {
	if id := uiThread.Load(); id != 0 {
		_, _, _ = procEnumThreadWindows.Call(uintptr(id), closeWindow(), 0)
	}
}

// available reports whether a dialog can be seen: an SSH session or a
// service runs on a window station nobody looks at, where a dialog would
// wait forever.
func available() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_CLIENT") != "" {
		return false
	}
	station, _, _ := procGetProcessWindowStation.Call()
	if station == 0 {
		return false
	}
	var flags userObjectFlags
	var needed uint32
	r, _, _ := procGetUserObjectInformation.Call(station, uoiFlags, uintptr(unsafe.Pointer(&flags)), unsafe.Sizeof(flags), uintptr(unsafe.Pointer(&needed)))
	return r != 0 && flags.flags&wsfVisible != 0
}

// owner picks the window the dialog sits on. Windows Terminal hosts the
// console in a hidden pseudo window; the foreground window is then the
// terminal the person is typing in.
func owner() uintptr {
	console, _, _ := procGetConsoleWindow.Call()
	if console != 0 {
		if visible, _, _ := procIsWindowVisible.Call(console); visible != 0 {
			return console
		}
	}
	fg, _, _ := procGetForegroundWindow.Call()
	if fg != 0 {
		return fg
	}
	return console
}

// onUIThread runs fn on one locked OS thread with COM initialized for the
// shell dialogs.
func onUIThread(fn func() error) error {
	if !available() {
		return serialcmd.ErrPickerUnavailable
	}
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err == nil {
			defer procCoUninitialize.Call()
		}
		id, _, _ := procGetCurrentThreadID.Call()
		uiThread.Store(uint32(id))
		defer uiThread.Store(0)
		done <- fn()
	}()
	return <-done
}

func utf16Ptr(s string) *uint16 {
	if s == "" {
		return nil
	}
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	return p
}

var allFilesFilter = utf16Ptr("All files (*.*)\x00*.*\x00")

func dialogError() error {
	code, _, _ := procCommDlgExtendedError.Call()
	if code == 0 {
		return serialcmd.ErrPickCancelled
	}
	return fmt.Errorf("file dialog failed: error 0x%x", code)
}

// OpenFiles shows a multi-select Open dialog.
func (Dialog) OpenFiles(dir string) ([]string, error) {
	var files []string
	err := onUIThread(func() error {
		buf := make([]uint16, fileBufferChars)
		ofn := openFileName{
			owner:      owner(),
			filter:     allFilesFilter,
			file:       &buf[0],
			maxFile:    uint32(len(buf)),
			initialDir: utf16Ptr(dir),
			title:      utf16Ptr("sio: choose file(s) to send to the device"),
			flags:      ofnExplorer | ofnAllowMultiSelect | ofnFileMustExist | ofnPathMustExist | ofnNoChangeDir | ofnHideReadOnly,
		}
		ofn.structSize = uint32(unsafe.Sizeof(ofn))
		_, _, _ = procSetForegroundWindow.Call(ofn.owner)
		r, _, _ := procGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
		if r == 0 {
			return dialogError()
		}
		files = splitMultiSelect(buf)
		return nil
	})
	return files, err
}

// splitMultiSelect reads "dir\0name\0name\0\0", or one full path.
func splitMultiSelect(buf []uint16) []string {
	var parts []string
	start := 0
	for i, c := range buf {
		if c != 0 {
			continue
		}
		if i == start {
			break
		}
		parts = append(parts, syscall.UTF16ToString(buf[start:i]))
		start = i + 1
	}
	if len(parts) <= 1 {
		return parts
	}
	files := make([]string, 0, len(parts)-1)
	for _, name := range parts[1:] {
		files = append(files, filepath.Join(parts[0], name))
	}
	return files
}

// SaveFile shows a Save As dialog filled with the device file name. The
// dialog asks before replacing a file.
func (Dialog) SaveFile(dir, name string) (string, error) {
	var path string
	err := onUIThread(func() error {
		buf := make([]uint16, fileBufferChars)
		copy(buf, windows.StringToUTF16(name))
		ofn := openFileName{
			owner:      owner(),
			filter:     allFilesFilter,
			file:       &buf[0],
			maxFile:    uint32(len(buf)),
			initialDir: utf16Ptr(dir),
			title:      utf16Ptr("sio: save the file from the device"),
			flags:      ofnExplorer | ofnOverwritePrompt | ofnPathMustExist | ofnNoChangeDir | ofnHideReadOnly,
		}
		ofn.structSize = uint32(unsafe.Sizeof(ofn))
		_, _, _ = procSetForegroundWindow.Call(ofn.owner)
		r, _, _ := procGetSaveFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
		if r == 0 {
			return dialogError()
		}
		path = syscall.UTF16ToString(buf)
		return nil
	})
	return path, err
}

// PickFolder shows the shell folder browser, starting at dir.
func (Dialog) PickFolder(dir string) (string, error) {
	var path string
	err := onUIThread(func() error {
		start := utf16Ptr(dir)
		display := make([]uint16, windows.MAX_PATH)
		info := browseInfo{
			owner:       owner(),
			displayName: &display[0],
			title:       utf16Ptr("sio: choose the folder for the files from the device"),
			flags:       bifReturnOnlyFSDirs | bifNewDialogStyle | bifEditBox,
			callback:    browseCallback(),
			param:       uintptr(unsafe.Pointer(start)),
		}
		_, _, _ = procSetForegroundWindow.Call(info.owner)
		pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&info)))
		runtime.KeepAlive(start)
		if pidl == 0 {
			return serialcmd.ErrPickCancelled
		}
		defer procCoTaskMemFree.Call(pidl)
		buf := make([]uint16, 32*1024)
		r, _, _ := procSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&buf[0])))
		if r == 0 {
			return errors.New("the chosen folder is not on the file system")
		}
		path = syscall.UTF16ToString(buf)
		return nil
	})
	if err == nil && strings.TrimSpace(path) == "" {
		err = serialcmd.ErrPickCancelled
	}
	return path, err
}
