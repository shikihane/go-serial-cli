//go:build !windows

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

func configureBackgroundProcess(cmd *exec.Cmd) {
}

func isElevationRequired(err error) bool {
	return false
}

func runSetupCOperationsElevatedInWindow(ops []setupCOperation) error {
	return errors.New("elevated setupc is only supported on Windows")
}

func pauseAdminWindow() {
	fmt.Fprintln(os.Stdout, "Press Enter to exit...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}
