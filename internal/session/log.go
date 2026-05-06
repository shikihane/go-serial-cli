package session

import (
	"os"
	"path/filepath"
	"time"
)

func (s Store) WorkerLogPath(name string) string {
	return filepath.Join(s.SessionDir(name), "worker.log")
}

func (s Store) HistoryPath(name string) string {
	return filepath.Join(s.SessionDir(name), "history.log")
}

func AppendLog(path string, line string) error {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(time.Now().Local().Format(time.RFC3339) + " " + line + "\n")
	return err
}
