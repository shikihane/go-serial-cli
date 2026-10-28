package cli_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"go-serial-cli/internal/cli"
	"go-serial-cli/internal/session"
)

func leaseCapableSession(t *testing.T) (session.Store, session.State) {
	t.Helper()
	store := session.Store{Dir: t.TempDir()}
	state := session.State{
		Name:           "dev1",
		Port:           "COM3",
		Baud:           115200,
		Status:         session.StatusConfigured,
		ControlAddress: "127.0.0.1:1",
		WorkerPID:      4242,
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	startWorkerLog(t, store, "worker start mode=session pid=4242 control=127.0.0.1:1 portlease=1")
	return store, state
}

func TestPauseReleasesThePortAndResumeTakesItBack(t *testing.T) {
	store, _ := leaseCapableSession(t)
	var releasedAddress, acquiredAddress string
	var out bytes.Buffer
	app := cli.New(cli.AppDeps{
		Store:            store,
		IsProcessRunning: func(pid int) bool { return true },
		ReleaseSessionPort: func(address string) error {
			releasedAddress = address
			return nil
		},
		AcquireSessionPort: func(address string) error {
			acquiredAddress = address
			return nil
		},
	})

	if err := app.Run([]string{"pause", "dev1"}, &out); err != nil {
		t.Fatalf("pause returned error: %v", err)
	}
	if releasedAddress != "127.0.0.1:1" {
		t.Fatalf("released address = %q, want the control address", releasedAddress)
	}
	if !strings.Contains(out.String(), "paused dev1: COM3 released for another program") {
		t.Fatalf("pause output = %q", out.String())
	}
	state, err := store.Load("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Paused || state.Status != session.StatusPaused {
		t.Fatalf("state after pause = %#v, want paused", state)
	}

	out.Reset()
	if err := app.Run([]string{"resume", "dev1"}, &out); err != nil {
		t.Fatalf("resume returned error: %v", err)
	}
	if acquiredAddress != "127.0.0.1:1" {
		t.Fatalf("acquired address = %q, want the control address", acquiredAddress)
	}
	if !strings.Contains(out.String(), "resumed dev1: COM3 open at 115200") {
		t.Fatalf("resume output = %q", out.String())
	}
	state, err = store.Load("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if state.Paused || state.Status != session.StatusConfigured {
		t.Fatalf("state after resume = %#v, want configured", state)
	}
}

func TestResumeKeepsTheSessionPausedWhenThePortIsStillHeld(t *testing.T) {
	store, _ := leaseCapableSession(t)
	if err := store.MarkPortReleased("dev1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(session.State{
		Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusPaused,
		Paused: true, ControlAddress: "127.0.0.1:1", WorkerPID: 4242,
	}); err != nil {
		t.Fatal(err)
	}
	app := cli.New(cli.AppDeps{
		Store:            store,
		IsProcessRunning: func(pid int) bool { return true },
		AcquireSessionPort: func(address string) error {
			return errors.New("Access is denied.")
		},
	})

	err := app.Run([]string{"resume", "dev1"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "still held") {
		t.Fatalf("resume error = %v, want a busy port refusal", err)
	}
	state, loadErr := store.Load("dev1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !state.Paused {
		t.Fatal("resume cleared the pause even though the port stayed with the other program")
	}
	if !store.PortReleased("dev1") {
		t.Fatal("resume dropped the released marker while the port stayed with the other program")
	}
}

func TestPauseRefusesAnOlderWorker(t *testing.T) {
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusConfigured,
		ControlAddress: "127.0.0.1:1", WorkerPID: 4242,
	}); err != nil {
		t.Fatal(err)
	}
	startWorkerLog(t, store, "worker start mode=session pid=4242 control=127.0.0.1:1")
	app := cli.New(cli.AppDeps{
		Store:            store,
		IsProcessRunning: func(pid int) bool { return true },
		ReleaseSessionPort: func(address string) error {
			t.Fatal("an older worker must not receive a release request")
			return nil
		},
	})

	err := app.Run([]string{"pause", "dev1"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "predates port release") {
		t.Fatalf("pause error = %v, want an old worker refusal", err)
	}
	state, loadErr := store.Load("dev1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state.Paused {
		t.Fatal("pause marked the session paused even though it could not free the port")
	}
}

func TestPauseRefusesASharingSession(t *testing.T) {
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{
		Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusSharing,
		VirtualPorts: []string{"COM20"}, HubPorts: []string{"CNCB20"},
		ControlAddress: "127.0.0.1:1", WorkerPID: 4242,
	}); err != nil {
		t.Fatal(err)
	}
	app := cli.New(cli.AppDeps{
		Store:            store,
		IsProcessRunning: func(pid int) bool { return true },
		ReleaseSessionPort: func(address string) error {
			t.Fatal("a sharing session must be stopped instead of released")
			return nil
		},
	})

	err := app.Run([]string{"pause", "dev1"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "sharing session") {
		t.Fatalf("pause error = %v, want a sharing session refusal", err)
	}
}

func TestPauseWithoutAWorkerOnlyMarksTheSession(t *testing.T) {
	store := session.Store{Dir: t.TempDir()}
	if err := store.Save(session.State{Name: "dev1", Port: "COM3", Baud: 115200}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkPortReleased("dev1"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	app := cli.New(cli.AppDeps{Store: store})

	if err := app.Run([]string{"pause", "dev1"}, &out); err != nil {
		t.Fatalf("pause returned error: %v", err)
	}
	if !strings.Contains(out.String(), "paused dev1: no worker running") {
		t.Fatalf("pause output = %q", out.String())
	}
	state, err := store.Load("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Paused {
		t.Fatal("pause did not mark the session")
	}
	if store.PortReleased("dev1") {
		t.Fatal("a stale released marker survived pause without a worker")
	}
}
