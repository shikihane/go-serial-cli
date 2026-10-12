package session

import (
	"os"
	"path/filepath"
	"strings"
)

// TransferDirPath holds the folder last used for a shell file transfer.
// It is kept apart from state.json, which the worker rewrites.
func (s Store) TransferDirPath(name string) string {
	return filepath.Join(s.SessionDir(name), "transfer_dir")
}

// LastTransferDir returns the folder last used for a shell file transfer,
// or "" when none was saved or it no longer exists.
func (s Store) LastTransferDir(name string) string {
	data, err := os.ReadFile(s.TransferDirPath(name))
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(data))
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ""
	}
	return dir
}

// SaveLastTransferDir records the folder used for a shell file transfer.
func (s Store) SaveLastTransferDir(name string, dir string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if err := os.MkdirAll(s.SessionDir(name), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.TransferDirPath(name), []byte(dir+"\n"), 0o644)
}
