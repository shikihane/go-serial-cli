package session_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-serial-cli/internal/session"
)

func TestStoreUsesPerSessionLogPaths(t *testing.T) {
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

func TestAppendLogCreatesParentAndAddsTimestampedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev1", "worker.log")

	if err := session.AppendLog(path, "worker start mode=share"); err != nil {
		t.Fatalf("AppendLog returned error: %v", err)
	}
	if err := session.AppendLog(path, "worker exit"); err != nil {
		t.Fatalf("AppendLog returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want 2 in %q", len(lines), string(data))
	}
	for _, line := range lines {
		if len(line) < len("2006-01-02T15:04:05") || line[4] != '-' || !strings.Contains(line, " worker ") {
			t.Fatalf("log line %q is not timestamped as expected", line)
		}
	}
}
