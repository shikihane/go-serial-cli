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

func createVirtualPortsPlatform(pairs []VirtualPortPair, ops []setupCOperation) error {
	return runSetupCOperationsWithElevationFallback(ops, nil, runSetupCDirect, runSetupCOperationsElevatedInWindow)
}

func removeVirtualPortsPlatform(pairs []VirtualPortPair) error {
	var ops []setupCOperation
	for _, pair := range pairs {
		id := portPairID(pair)
		if id == "" {
			return fmt.Errorf("cannot infer com0com pair id for %s/%s", pair.Public, pair.Hub)
		}
		ops = append(ops, setupCOperation{
			Description: "remove virtual port " + pair.Public,
			Args:        []string{"remove", id},
		})
	}
	return runSetupCOperationsWithElevationFallback(ops, nil, runSetupCDirect, runSetupCOperationsElevatedInWindow)
}

func clearSharePortsPlatform() error {
	return errors.New("clear --share is only supported on Windows")
}

func releaseStaleCOMNameReservationsForOperation(op setupCOperation, out io.Writer) error {
	return nil
}

func runSetupCOperationsElevatedInWindow(ops []setupCOperation) error {
	return errors.New("elevated setupc is only supported on Windows")
}

func runPnpRemoveParents(parents []string, out io.Writer) error {
	return errors.New("pnp remove is only supported on Windows")
}

func pauseAdminWindow() {
	fmt.Fprintln(os.Stdout, "Press Enter to exit...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}
