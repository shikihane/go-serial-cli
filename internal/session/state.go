package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	StatusConfigured = "configured"
	StatusSharing    = "sharing"
	StatusPaused     = "paused"
	StatusStopped    = "stopped"
	StatusTCP        = "tcp"
)

type State struct {
	Name           string   `json:"name"`
	Port           string   `json:"port"`
	Baud           int      `json:"baud"`
	Status         string   `json:"status"`
	Paused         bool     `json:"paused"`
	VirtualPorts   []string `json:"virtual_ports,omitempty"`
	HubPorts       []string `json:"hub_ports,omitempty"`
	TCPAddress     string   `json:"tcp_address,omitempty"`
	ControlAddress string   `json:"control_address,omitempty"`
	CheckOffset    int64    `json:"check_offset,omitempty"`
	WorkerPID      int      `json:"worker_pid,omitempty"`
	HubPID         int      `json:"hub_pid,omitempty"`
}

type Store struct {
	Dir string
}

func DefaultStore() (Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return Store{}, err
	}
	return Store{Dir: filepath.Join(dir, "gs")}, nil
}

func (s Store) Save(state State) error {
	if err := ValidateName(state.Name); err != nil {
		return err
	}
	if state.Port == "" {
		return errors.New("port is required")
	}
	if state.Baud <= 0 {
		return errors.New("baud must be positive")
	}
	if state.Status == "" {
		state.Status = StatusConfigured
	}
	if err := os.MkdirAll(s.SessionDir(state.Name), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(state.Name), data, 0o644)
}

func (s Store) Load(name string) (State, error) {
	if err := ValidateName(name); err != nil {
		return State{}, err
	}
	data, err := os.ReadFile(s.path(name))
	if err != nil {
		return State{}, errors.New("no serial session named " + name + "; run sio open <session> <port> first")
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, err
	}
	if state.Name == "" {
		state.Name = name
	}
	if state.Name != name || state.Port == "" || state.Baud <= 0 {
		return State{}, errors.New("saved serial session is invalid")
	}
	if state.Status == "" {
		state.Status = StatusConfigured
	}
	return state, nil
}

func (s Store) List() ([]State, error) {
	root := s.sessionsRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var states []State
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		state, err := s.Load(entry.Name())
		if err != nil {
			continue
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool {
		return states[i].Name < states[j].Name
	})
	return states, nil
}

func (s Store) Stop(name string) error {
	state, err := s.Load(name)
	if err != nil {
		return err
	}
	state.Status = StatusStopped
	state.Paused = false
	state.VirtualPorts = nil
	state.HubPorts = nil
	state.TCPAddress = ""
	state.ControlAddress = ""
	state.CheckOffset = 0
	state.WorkerPID = 0
	state.HubPID = 0
	return s.Save(state)
}

func (s Store) Remove(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	return os.RemoveAll(s.SessionDir(name))
}

func (s Store) CachePath(name string) string {
	return filepath.Join(s.SessionDir(name), "cache.log")
}

func (s Store) SessionDir(name string) string {
	return filepath.Join(s.sessionsRoot(), name)
}

func ValidateName(name string) error {
	if name == "" {
		return errors.New("session name is required")
	}
	if name == "." || name == ".." {
		return errors.New("session name must not be . or ..")
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return errors.New("session name may only contain letters, digits, dots, dashes, and underscores")
	}
	if strings.Contains(name, "..") {
		return errors.New("session name must not contain ..")
	}
	return nil
}

func (s Store) sessionsRoot() string {
	return filepath.Join(s.Dir, "sessions")
}

func (s Store) path(name string) string {
	return filepath.Join(s.SessionDir(name), "state.json")
}
