//go:build windows

package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

const (
	testDetachedProcess     uint32 = 0x00000008
	testCreateNewProcessGrp uint32 = 0x00000200
)

func TestBackgroundProcessesDetachFromConsole(t *testing.T) {
	cmd := exec.Command("gs.exe", "worker", "run", "dev1")

	configureBackgroundProcess(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	flags := cmd.SysProcAttr.CreationFlags
	for _, flag := range []uint32{testDetachedProcess, testCreateNewProcessGrp} {
		if flags&flag == 0 {
			t.Fatalf("CreationFlags = %#x, missing %#x", flags, flag)
		}
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("HideWindow = false, want true")
	}
}

func TestElevationRequiredErrorIsDetected(t *testing.T) {
	err := &os.PathError{Op: "fork/exec", Path: "setupc.exe", Err: windows.ERROR_ELEVATION_REQUIRED}

	if !isElevationRequired(err) {
		t.Fatal("isElevationRequired returned false")
	}
}

func TestSetupCElevationFallbackRunsElevatedWhenDirectRunNeedsElevation(t *testing.T) {
	var directOps []setupCOperation
	var elevatedOps []setupCOperation
	ops := []setupCOperation{
		{Description: "create virtual port COM20", Args: []string{"install", "20", "PortName=COM20", "-"}},
		{Description: "create virtual port COM21", Args: []string{"install", "21", "PortName=COM21", "-"}},
	}

	err := runSetupCOperationsWithElevationFallback(ops, io.Discard,
		func(args []string, out io.Writer) error {
			directOps = append(directOps, setupCOperation{Args: append([]string(nil), args...)})
			return &os.PathError{Op: "fork/exec", Path: "setupc.exe", Err: windows.ERROR_ELEVATION_REQUIRED}
		},
		func(ops []setupCOperation) error {
			elevatedOps = append([]setupCOperation(nil), ops...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("runSetupCOperationsWithElevationFallback returned error: %v", err)
	}
	if !reflect.DeepEqual(directOps, []setupCOperation{{Args: ops[0].Args}}) {
		t.Fatalf("direct ops = %#v", directOps)
	}
	if !reflect.DeepEqual(elevatedOps, ops) {
		t.Fatalf("elevated ops = %#v, want %#v", elevatedOps, ops)
	}
}

func TestSetupCElevationFallbackRunsElevatedWhenDirectRunExitsNonZero(t *testing.T) {
	var elevatedOps []setupCOperation
	ops := []setupCOperation{
		{Description: "remove virtual port COM90", Args: []string{"remove", "90"}},
	}

	err := runSetupCOperationsWithElevationFallback(ops, io.Discard,
		func(args []string, out io.Writer) error {
			return &exec.ExitError{}
		},
		func(ops []setupCOperation) error {
			elevatedOps = append([]setupCOperation(nil), ops...)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("runSetupCOperationsWithElevationFallback returned error: %v", err)
	}
	if !reflect.DeepEqual(elevatedOps, ops) {
		t.Fatalf("elevated ops = %#v, want %#v", elevatedOps, ops)
	}
}

func TestSetupCInstallReleasesStaleCOMNameReservation(t *testing.T) {
	oldSerialPortDevice := serialPortDeviceFunc
	oldRelease := releaseStaleCOMNameFunc
	t.Cleanup(func() {
		serialPortDeviceFunc = oldSerialPortDevice
		releaseStaleCOMNameFunc = oldRelease
	})
	serialPortDeviceFunc = func(port string) (string, error) {
		return "", nil
	}
	var released []string
	releaseStaleCOMNameFunc = func(port string) (bool, error) {
		released = append(released, port)
		return true, nil
	}
	op := setupCOperation{Args: []string{"install", "90", "PortName=COM90", "PortName=CNCB90"}}

	var out strings.Builder
	if err := releaseStaleCOMNameReservationsForOperation(op, &out); err != nil {
		t.Fatalf("releaseStaleCOMNameReservationsForOperation returned error: %v", err)
	}
	if !reflect.DeepEqual(released, []string{"COM90"}) {
		t.Fatalf("released ports = %#v, want COM90 only", released)
	}
	if !strings.Contains(out.String(), "Released stale COM name reservation COM90") {
		t.Fatalf("output = %q, want release message", out.String())
	}
}

func TestSetupCInstallDoesNotReleaseMappedCOMNameReservation(t *testing.T) {
	oldSerialPortDevice := serialPortDeviceFunc
	oldRelease := releaseStaleCOMNameFunc
	t.Cleanup(func() {
		serialPortDeviceFunc = oldSerialPortDevice
		releaseStaleCOMNameFunc = oldRelease
	})
	serialPortDeviceFunc = func(port string) (string, error) {
		return `\Device\Serial2`, nil
	}
	releaseStaleCOMNameFunc = func(port string) (bool, error) {
		t.Fatalf("releaseStaleCOMName should not be called for mapped port %s", port)
		return false, nil
	}
	op := setupCOperation{Args: []string{"install", "5", "PortName=COM5", "PortName=CNCB5"}}

	if err := releaseStaleCOMNameReservationsForOperation(op, io.Discard); err != nil {
		t.Fatalf("releaseStaleCOMNameReservationsForOperation returned error: %v", err)
	}
}

func TestSetupCCommandRunsFromSetupCInstallDirectory(t *testing.T) {
	setupc := filepath.Join(t.TempDir(), "com0com", "setupc.exe")
	cmd := newSetupCCommand(setupc, []string{"install", "20"})

	if cmd.Dir != filepath.Dir(setupc) {
		t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, filepath.Dir(setupc))
	}
}

func TestCom0comPortInstanceIDMapsPublicCOMToCNCA(t *testing.T) {
	tests := map[string]string{
		"COM90":  `COM0COM\PORT\CNCA90`,
		"CNCA90": `COM0COM\PORT\CNCA90`,
		"CNCB90": `COM0COM\PORT\CNCB90`,
	}
	for port, want := range tests {
		if got := com0comPortInstanceID(port); got != want {
			t.Fatalf("com0comPortInstanceID(%q) = %q, want %q", port, got, want)
		}
	}
}

func TestVirtualPortInstallOperationNamesBothPairSides(t *testing.T) {
	op, err := setupCInstallOperation(VirtualPortPair{Public: "COM98", Hub: "CNCB98"})
	if err != nil {
		t.Fatalf("setupCInstallOperation returned error: %v", err)
	}

	want := setupCOperation{
		Description: "create virtual port COM98",
		Args:        []string{"install", "98", "PortName=COM98", "PortName=CNCB98"},
	}
	if !reflect.DeepEqual(op, want) {
		t.Fatalf("operation = %#v, want %#v", op, want)
	}
}
