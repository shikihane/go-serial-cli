//go:build windows

package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func TestSetupCCommandRunsFromSetupCInstallDirectory(t *testing.T) {
	setupc := filepath.Join(t.TempDir(), "com0com", "setupc.exe")
	cmd := newSetupCCommand(setupc, []string{"install", "20"})

	if cmd.Dir != filepath.Dir(setupc) {
		t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, filepath.Dir(setupc))
	}
}

func TestHubCommandRunsExternalHub4comFromPath(t *testing.T) {
	hub4com := filepath.Join(t.TempDir(), "hub4com.exe")
	cmd := newHub4comCommand(hub4com, []string{"--route=All:All"})

	if cmd.Path != hub4com {
		t.Fatalf("cmd.Path = %q, want %q", cmd.Path, hub4com)
	}
	if cmd.Dir != filepath.Dir(hub4com) {
		t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, filepath.Dir(hub4com))
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

func TestHubCommandArgsRouteAllPortsToAllPorts(t *testing.T) {
	got := hubCommandArgs(HubOptions{
		PhysicalPort: "COM3",
		Baud:         3000000,
		HubPorts:     []string{"CNCB20", "CNCB21"},
	})

	want := []string{"--route=All:All", "--baud=3000000", "--octs=off", `\\.\COM3`, `\\.\CNCB20`, `\\.\CNCB21`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}
