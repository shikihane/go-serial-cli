//go:build windows

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	detachedProcess       uint32 = 0x00000008
	createNewProcessGroup uint32 = 0x00000200
	seeMaskNoCloseProcess uint32 = 0x00000040
	virtualPortRemoveWait        = 10 * time.Second
)

var procShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

var (
	pnpDeviceParentFunc              = pnpDeviceParent
	com0comParentIDsFunc             = com0comParentIDs
	serialPortDeviceFunc             = serialPortDevice
	releaseStaleCOMNameFunc          = releaseStaleCOMName
	removeCom0comParentsElevatedFunc = removeCom0comParentsElevated
	runSetupCOperationsElevatedFunc  = runSetupCOperationsElevatedInWindow
)

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
	if errors.Is(err, windows.ERROR_ELEVATION_REQUIRED) {
		return true
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

func createVirtualPortsPlatform(pairs []VirtualPortPair, ops []setupCOperation) error {
	if len(ops) == 0 {
		return nil
	}
	if err := ensureNoNonCom0comPortCollision(pairs); err != nil {
		return err
	}
	needsCleanup, err := virtualPortsNeedPreClean(pairs)
	if err != nil {
		return err
	}
	if needsCleanup {
		if err := removeVirtualPortsPlatform(pairs); err != nil {
			return fmt.Errorf("pre-clean virtual ports before install: %w", err)
		}
	}
	if err := ensureVirtualPortsAvailableForInstall(pairs); err != nil {
		return err
	}
	return runSetupCOperationsElevatedFunc(ops)
}

func virtualPortsNeedPreClean(pairs []VirtualPortPair) (bool, error) {
	seen := map[string]bool{}
	for _, pair := range pairs {
		for _, port := range []string{pair.Public, pair.Hub} {
			port = strings.ToUpper(strings.TrimSpace(port))
			if port == "" || seen[port] {
				continue
			}
			seen[port] = true
			device, err := serialPortDeviceFunc(port)
			if err != nil {
				return false, fmt.Errorf("check serial port %s before install cleanup: %w", port, err)
			}
			if device == "" {
				continue
			}
			if isCom0comSerialDevice(device) {
				return true, nil
			}
			return false, fmt.Errorf("serial port %s already exists and is not a com0com virtual port (%s)", port, device)
		}
	}
	for _, pair := range pairs {
		parent, err := com0comPairParent(pair)
		if err == nil && parent != "" {
			return true, nil
		}
	}
	return false, nil
}

func removeVirtualPortsPlatform(pairs []VirtualPortPair) error {
	parents := make([]string, 0, len(pairs))
	seen := map[string]bool{}
	for _, pair := range pairs {
		parent, err := com0comPairParent(pair)
		if err == nil && parent != "" {
			if !seen[parent] {
				seen[parent] = true
				parents = append(parents, parent)
			}
			continue
		}
		if com0comPortInstanceID(pair.Public) == "" && com0comPortInstanceID(pair.Hub) == "" {
			return fmt.Errorf("cannot infer com0com pair id for %s/%s", pair.Public, pair.Hub)
		}
	}
	removeErr := removeVirtualPortsWithSetupC(pairs)
	if err := waitForVirtualPortsRemoved(pairs); err == nil {
		return nil
	}
	if len(parents) > 0 {
		removeErr = errors.Join(removeErr, removeCom0comParentsElevatedFunc(parents))
	}
	if err := waitForVirtualPortsRemoved(pairs); err != nil {
		if removeErr != nil {
			return errors.Join(removeErr, err)
		}
		return err
	}
	return nil
}

func removeVirtualPortsWithSetupC(pairs []VirtualPortPair) error {
	ops := make([]setupCOperation, 0, len(pairs))
	for _, pair := range pairs {
		op, err := setupCRemoveOperation(pair)
		if err != nil {
			return err
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return nil
	}
	return runSetupCOperationsElevatedFunc(ops)
}

func clearSharePortsPlatform() error {
	parents, err := com0comParentIDsFunc()
	if err != nil {
		return err
	}
	if len(parents) == 0 {
		return nil
	}
	removeErr := removeCom0comParentsElevatedFunc(parents)
	return waitForCom0comParentsCleared(removeErr)
}

func waitForCom0comParentsCleared(removeErr error) error {
	deadline := time.Now().Add(virtualPortRemoveWait)
	for {
		remaining, err := com0comParentIDsFunc()
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			stillExistsErr := fmt.Errorf("com0com parents still exist after clear: %s", strings.Join(remaining, ", "))
			if removeErr != nil {
				return errors.Join(removeErr, stillExistsErr)
			}
			return stillExistsErr
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func ensureVirtualPortsAvailableForInstall(pairs []VirtualPortPair) error {
	var checkErr error
	seen := map[string]bool{}
	for _, pair := range pairs {
		for _, port := range []string{pair.Public, pair.Hub} {
			port = strings.ToUpper(strings.TrimSpace(port))
			if port == "" || seen[port] {
				continue
			}
			seen[port] = true
			device, err := serialPortDeviceFunc(port)
			if err != nil {
				checkErr = errors.Join(checkErr, fmt.Errorf("check serial port %s: %w", port, err))
				continue
			}
			if device == "" {
				continue
			}
			if isCom0comSerialDevice(device) {
				checkErr = errors.Join(checkErr, fmt.Errorf("virtual port %s still exists after cleanup; close programs using it and run sio clear --share", port))
				continue
			}
			checkErr = errors.Join(checkErr, fmt.Errorf("serial port %s already exists and is not a com0com virtual port (%s)", port, device))
		}
	}
	return checkErr
}

func ensureNoNonCom0comPortCollision(pairs []VirtualPortPair) error {
	var checkErr error
	seen := map[string]bool{}
	for _, pair := range pairs {
		for _, port := range []string{pair.Public, pair.Hub} {
			port = strings.ToUpper(strings.TrimSpace(port))
			if port == "" || seen[port] {
				continue
			}
			seen[port] = true
			device, err := serialPortDeviceFunc(port)
			if err != nil {
				checkErr = errors.Join(checkErr, fmt.Errorf("check serial port %s: %w", port, err))
				continue
			}
			if device != "" && !isCom0comSerialDevice(device) {
				checkErr = errors.Join(checkErr, fmt.Errorf("serial port %s already exists and is not a com0com virtual port (%s)", port, device))
			}
		}
	}
	return checkErr
}

func ensureVirtualPortsRemoved(pairs []VirtualPortPair) error {
	var checkErr error
	seen := map[string]bool{}
	for _, pair := range pairs {
		for _, port := range []string{pair.Public, pair.Hub} {
			port = strings.ToUpper(strings.TrimSpace(port))
			if port == "" || seen[port] {
				continue
			}
			seen[port] = true
			device, err := serialPortDeviceFunc(port)
			if err != nil {
				checkErr = errors.Join(checkErr, fmt.Errorf("check serial port %s after removal: %w", port, err))
				continue
			}
			if device != "" {
				checkErr = errors.Join(checkErr, fmt.Errorf("virtual port %s still exists after removal; Windows may still be finalizing com0com device removal or another process may be holding it", port))
			}
		}
	}
	return checkErr
}

func waitForVirtualPortsRemoved(pairs []VirtualPortPair) error {
	deadline := time.Now().Add(virtualPortRemoveWait)
	for {
		err := ensureVirtualPortsRemoved(pairs)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func com0comPairParent(pair VirtualPortPair) (string, error) {
	for _, port := range []string{pair.Public, pair.Hub} {
		id := com0comPortInstanceID(port)
		if id == "" {
			continue
		}
		parent, err := pnpDeviceParentFunc(id)
		if err == nil && parent != "" {
			return parent, nil
		}
	}
	return "", fmt.Errorf("cannot find com0com parent for %s/%s", pair.Public, pair.Hub)
}

func com0comPortInstanceID(port string) string {
	suffix := numericSuffix(port)
	if suffix == "" {
		return ""
	}
	upper := strings.ToUpper(port)
	switch {
	case strings.HasPrefix(upper, "CNCB"):
		return `COM0COM\PORT\CNCB` + suffix
	case strings.HasPrefix(upper, "CNCA"):
		return `COM0COM\PORT\CNCA` + suffix
	case strings.HasPrefix(upper, "COM"):
		return `COM0COM\PORT\CNCA` + suffix
	default:
		return ""
	}
}

func pnpDeviceParent(instanceID string) (string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command",
		"(Get-PnpDeviceProperty -InstanceId $args[0] -KeyName 'DEVPKEY_Device_Parent' -ErrorAction Stop).Data",
		instanceID,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func com0comParentIDs() ([]string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command",
		"$devices = @(Get-PnpDevice -Class CNCPorts -ErrorAction SilentlyContinue); "+
			"$devices | "+
			"Where-Object { $_.InstanceId -like 'ROOT\\CNCPORTS\\*' } | "+
			"ForEach-Object { $_.InstanceId }; "+
			"exit 0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.TrimSpace(string(out)) == "" && isElevationRequired(err) {
			return nil, nil
		}
		return nil, err
	}
	var parents []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		parent := strings.TrimSpace(line)
		if parent == "" {
			continue
		}
		key := strings.ToUpper(parent)
		if seen[key] {
			continue
		}
		seen[key] = true
		parents = append(parents, parent)
	}
	return parents, nil
}

func serialPortDevice(port string) (string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DEVICEMAP\SERIALCOMM`, registry.READ)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer key.Close()

	want := strings.ToUpper(strings.TrimSpace(port))
	names, err := key.ReadValueNames(0)
	if err != nil {
		return "", err
	}
	for _, name := range names {
		value, _, err := key.GetStringValue(name)
		if err != nil {
			return "", err
		}
		if strings.ToUpper(strings.TrimSpace(value)) != want {
			continue
		}
		return name, nil
	}
	return "", nil
}

func isCom0comSerialDevice(device string) bool {
	return strings.Contains(strings.ToLower(device), `\device\com0com`)
}

func releaseStaleCOMNameReservationsForOperation(op setupCOperation, out io.Writer) error {
	if len(op.Args) == 0 || strings.ToLower(op.Args[0]) != "install" {
		return nil
	}
	for _, port := range setupCOperationPortNames(op) {
		if !strings.HasPrefix(strings.ToUpper(port), "COM") {
			continue
		}
		device, err := serialPortDeviceFunc(port)
		if err != nil {
			return fmt.Errorf("check serial port %s before releasing stale COM reservation: %w", port, err)
		}
		if device != "" {
			continue
		}
		released, err := releaseStaleCOMNameFunc(port)
		if err != nil {
			return err
		}
		if released {
			_, _ = fmt.Fprintf(out, "Released stale COM name reservation %s\n", strings.ToUpper(port))
		}
	}
	return nil
}

func setupCOperationPortNames(op setupCOperation) []string {
	var ports []string
	for _, arg := range op.Args {
		name, value, ok := strings.Cut(arg, "=")
		if !ok || !strings.EqualFold(name, "PortName") {
			continue
		}
		value = strings.ToUpper(strings.TrimSpace(value))
		if value != "" {
			ports = append(ports, value)
		}
	}
	return ports
}

func releaseStaleCOMName(port string) (bool, error) {
	numberText := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(port)), "COM")
	number, err := strconv.Atoi(numberText)
	if err != nil || number <= 0 {
		return false, fmt.Errorf("invalid COM port %s", port)
	}
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\COM Name Arbiter`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer key.Close()
	data, _, err := key.GetBinaryValue("ComDB")
	if err != nil {
		return false, err
	}
	index := number - 1
	byteIndex := index / 8
	bit := byte(1 << uint(index%8))
	if byteIndex < 0 || byteIndex >= len(data) {
		return false, nil
	}
	if data[byteIndex]&bit == 0 {
		return false, nil
	}
	data[byteIndex] &^= bit
	if err := key.SetBinaryValue("ComDB", data); err != nil {
		return false, err
	}
	return true, nil
}

func removeCom0comParentsElevated(parents []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath, err := elevatedLogPath("pnp-remove")
	if err != nil {
		return err
	}
	defer os.Remove(logPath)
	args := append([]string{"admin", "pnp-remove"}, parents...)
	args = append(args, "--log", logPath)
	err = runElevatedAndWait(exe, windowsCommandLine(args))
	return attachElevatedLog(err, logPath)
}

func runPnpRemoveParents(parents []string, out io.Writer) error {
	var removeErr error
	for _, parent := range parents {
		if !strings.HasPrefix(strings.ToUpper(parent), `ROOT\CNCPORTS\`) {
			removeErr = errors.Join(removeErr, fmt.Errorf("refusing to remove non-com0com parent %s", parent))
			continue
		}
		if _, err := fmt.Fprintf(out, "remove com0com parent %s\n", parent); err != nil {
			return err
		}
		if err := runPnpUtil(out, "/disable-device", parent, "/force"); err != nil {
			removeErr = errors.Join(removeErr, fmt.Errorf("disable %s: %w", parent, err))
		}
		if err := runPnpUtil(out, "/remove-device", parent, "/subtree", "/force"); err != nil {
			removeErr = errors.Join(removeErr, fmt.Errorf("remove %s: %w", parent, err))
		}
	}
	return removeErr
}

func runPnpUtil(out io.Writer, args ...string) error {
	cmd := exec.Command("pnputil", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
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
	logPath, err := elevatedLogPath("setupc")
	if err != nil {
		return err
	}
	defer os.Remove(logPath)
	params := windowsCommandLine([]string{"admin", "setupc-batch", encoded, "--log", logPath})
	err = runElevatedAndWait(exe, params)
	return attachElevatedLog(err, logPath)
}

func elevatedLogPath(prefix string) (string, error) {
	file, err := os.CreateTemp("", "sio-"+prefix+"-*.log")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func attachElevatedLog(err error, logPath string) error {
	if err == nil {
		return nil
	}
	data, readErr := os.ReadFile(logPath)
	if readErr != nil || len(data) == 0 {
		return err
	}
	return fmt.Errorf("%w\n\nElevated command output:\n%s", err, strings.TrimSpace(string(data)))
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
