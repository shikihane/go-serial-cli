package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"go-serial-cli/internal/diag"
)

func startWorkerProcess(sessionName string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe, "worker", "run", sessionName)
	configureBackgroundProcess(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return 0, err
	}
	return pid, nil
}

func reserveControlAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", err
	}
	return address, nil
}

func waitForControlAddress(address string) error {
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			return conn.Close()
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("wait for session control %s: %w", address, lastErr)
}

func stopProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Kill(); err != nil {
		_ = process.Release()
		return fmt.Errorf("stop process %d: %w", pid, err)
	}
	return process.Release()
}

func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
		if err != nil {
			return false
		}
		return strings.Contains(string(out), fmt.Sprintf(`"%d"`, pid))
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	_ = process.Release()
	return err == nil
}

func createVirtualPorts(pairs []VirtualPortPair) error {
	var ops []setupCOperation
	for _, pair := range pairs {
		op, err := setupCInstallOperation(pair)
		if err != nil {
			return fmt.Errorf("cannot infer com0com pair id for %s/%s", pair.Public, pair.Hub)
		}
		ops = append(ops, op)
	}
	return createVirtualPortsPlatform(pairs, ops)
}

func setupCInstallOperation(pair VirtualPortPair) (setupCOperation, error) {
	id := portPairID(pair)
	if id == "" {
		return setupCOperation{}, errors.New("missing com0com pair id")
	}
	return setupCOperation{
		Description: "create virtual port " + pair.Public,
		Args:        []string{"install", id, "PortName=" + pair.Public, "PortName=" + pair.Hub},
	}, nil
}

func setupCRemoveOperation(pair VirtualPortPair) (setupCOperation, error) {
	id := portPairID(pair)
	if id == "" {
		return setupCOperation{}, errors.New("missing com0com pair id")
	}
	return setupCOperation{
		Description: "remove virtual port " + pair.Public,
		Args:        []string{"remove", id},
	}, nil
}

func removeVirtualPorts(pairs []VirtualPortPair) error {
	return removeVirtualPortsPlatform(pairs)
}

func clearSharePorts() error {
	return clearSharePortsPlatform()
}

type setupCRunner func(args []string, output io.Writer) error

type elevatedSetupCRunner func(ops []setupCOperation) error

type setupCOperation struct {
	Description string   `json:"description"`
	Args        []string `json:"args"`
}

func runSetupCOperationsWithElevationFallback(ops []setupCOperation, output io.Writer, run setupCRunner, elevate elevatedSetupCRunner) error {
	for i, op := range ops {
		err := run(op.Args, output)
		if err == nil {
			continue
		}
		if isElevationRequired(err) {
			return elevate(ops[i:])
		}
		if op.Description != "" {
			return fmt.Errorf("%s: %w", op.Description, err)
		}
		return err
	}
	return nil
}

func runSetupCDirect(args []string, output io.Writer) error {
	setupc, err := findSetupC()
	if err != nil {
		return err
	}
	cmd := newSetupCCommand(setupc, args)
	if output != nil {
		cmd.Stdout = output
		cmd.Stderr = output
	}
	return cmd.Run()
}

func newSetupCCommand(setupc string, args []string) *exec.Cmd {
	cmd := exec.Command(setupc, args...)
	cmd.Dir = filepath.Dir(setupc)
	return cmd
}

func encodeSetupCOperations(ops []setupCOperation) (string, error) {
	data, err := json.Marshal(ops)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func decodeSetupCOperations(encoded string) ([]setupCOperation, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	var ops []setupCOperation
	if err := json.Unmarshal(data, &ops); err != nil {
		return nil, err
	}
	return ops, nil
}

func findSetupC() (string, error) {
	if path, err := exec.LookPath("setupc.exe"); err == nil {
		return path, nil
	}
	candidates := []string{}
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if root := os.Getenv(env); root != "" {
			candidates = append(candidates,
				filepath.Join(root, "com0com", "setupc.exe"),
				filepath.Join(root, "com0com", "setupc", "setupc.exe"),
			)
		}
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", diag.MissingSetupCError()
}

func portPairID(pair VirtualPortPair) string {
	if suffix := numericSuffix(pair.Hub); suffix != "" {
		return suffix
	}
	return numericSuffix(pair.Public)
}

func numericSuffix(port string) string {
	re := regexp.MustCompile(`\d+$`)
	return re.FindString(port)
}

func windowsCOMPath(port string) string {
	if len(port) >= 4 && port[:4] == `\\.\` {
		return port
	}
	return `\\.\` + port
}
