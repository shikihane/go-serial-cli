package session_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go-serial-cli/internal/session"
)

func TestStoreSavesAndLoadsState(t *testing.T) {
	store := session.Store{Dir: t.TempDir()}
	state := session.State{Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusConfigured, Paused: true}

	if err := store.Save(state); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !reflect.DeepEqual(got, state) {
		t.Fatalf("state = %#v, want %#v", got, state)
	}
}

func TestStoreReportsMissingState(t *testing.T) {
	store := session.Store{Dir: filepath.Join(t.TempDir(), "missing")}

	if _, err := store.Load("dev1"); err == nil {
		t.Fatal("expected missing state error")
	}
}

func TestStoreRejectsUnsafeSessionName(t *testing.T) {
	store := session.Store{Dir: t.TempDir()}

	if err := store.Save(session.State{Name: "..", Port: "COM3", Baud: 115200}); err == nil {
		t.Fatal("expected unsafe session name error")
	}
	if _, err := store.Load("dev/1"); err == nil {
		t.Fatal("expected unsafe session name error")
	}
}

func TestStoreListsSessionsByName(t *testing.T) {
	store := session.Store{Dir: t.TempDir()}

	for _, state := range []session.State{
		{Name: "dev2", Port: "COM4", Baud: 9600, Status: session.StatusConfigured},
		{Name: "dev1", Port: "COM3", Baud: 115200, Status: session.StatusSharing, VirtualPorts: []string{"COM20"}},
	} {
		if err := store.Save(state); err != nil {
			t.Fatalf("Save(%s) returned error: %v", state.Name, err)
		}
	}

	got, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(List) = %d, want 2", len(got))
	}
	if got[0].Name != "dev1" || got[1].Name != "dev2" {
		t.Fatalf("names = %q, %q; want dev1, dev2", got[0].Name, got[1].Name)
	}
}

func TestStoreStopMarksSessionStoppedAndClearsSharedPorts(t *testing.T) {
	store := session.Store{Dir: t.TempDir()}
	state := session.State{
		Name:           "dev1",
		Port:           "COM3",
		Baud:           115200,
		Status:         session.StatusSharing,
		VirtualPorts:   []string{"COM20", "COM21"},
		HubPorts:       []string{"CNCB20", "CNCB21"},
		TCPAddress:     ":7001",
		ControlAddress: "127.0.0.1:7002",
		WorkerPID:      123,
		HubPID:         456,
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	if err := store.Stop("dev1"); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got.Status != session.StatusStopped {
		t.Fatalf("Status = %q, want %q", got.Status, session.StatusStopped)
	}
	if len(got.VirtualPorts) != 0 || len(got.HubPorts) != 0 || got.TCPAddress != "" || got.ControlAddress != "" || got.WorkerPID != 0 || got.HubPID != 0 {
		t.Fatalf("stopped state retained live resources: %#v", got)
	}
}

func TestStoreRemoveDeletesSessionDirectory(t *testing.T) {
	store := session.Store{Dir: t.TempDir()}
	for _, state := range []session.State{
		{Name: "dev1", Port: "COM3", Baud: 115200},
		{Name: "dev2", Port: "COM4", Baud: 9600},
	} {
		if err := store.Save(state); err != nil {
			t.Fatalf("Save(%s) returned error: %v", state.Name, err)
		}
	}
	if err := os.WriteFile(store.CachePath("dev1"), []byte("cached"), 0o644); err != nil {
		t.Fatalf("WriteFile cache returned error: %v", err)
	}

	if err := store.Remove("dev1"); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}

	if _, err := os.Stat(store.SessionDir("dev1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dev1 session dir still exists or stat failed differently: %v", err)
	}
	if _, err := store.Load("dev2"); err != nil {
		t.Fatalf("Load(dev2) returned error: %v", err)
	}
}

func TestStorePreservesControlAddress(t *testing.T) {
	store := session.Store{Dir: t.TempDir()}
	state := session.State{
		Name:           "dev1",
		Port:           "COM3",
		Baud:           115200,
		Status:         session.StatusConfigured,
		ControlAddress: "127.0.0.1:7002",
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	got, err := store.Load("dev1")
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !reflect.DeepEqual(got, state) {
		t.Fatalf("state = %#v, want %#v", got, state)
	}
}

func TestStoreUsesPerSessionCachePaths(t *testing.T) {
	store := session.Store{Dir: t.TempDir()}

	dev1 := store.CachePath("dev1")
	dev2 := store.CachePath("dev2")
	if dev1 == dev2 {
		t.Fatal("session cache paths should differ")
	}
	if filepath.Base(dev1) != "cache.log" {
		t.Fatalf("CachePath base = %q, want cache.log", filepath.Base(dev1))
	}
	if err := os.MkdirAll(filepath.Dir(dev1), 0o755); err != nil {
		t.Fatalf("MkdirAll returned error: %v", err)
	}
}

func TestStoreUsesPerSessionWorkerAndHubLogPaths(t *testing.T) {
	store := session.Store{Dir: t.TempDir()}

	if got := store.WorkerLogPath("dev1"); filepath.Base(got) != "worker.log" {
		t.Fatalf("WorkerLogPath base = %q, want worker.log", filepath.Base(got))
	}
	if got := store.HubLogPath("dev1"); filepath.Base(got) != "hub4com.log" {
		t.Fatalf("HubLogPath base = %q, want hub4com.log", filepath.Base(got))
	}
	if filepath.Dir(store.WorkerLogPath("dev1")) != store.SessionDir("dev1") {
		t.Fatalf("WorkerLogPath dir = %q, want %q", filepath.Dir(store.WorkerLogPath("dev1")), store.SessionDir("dev1"))
	}
}
