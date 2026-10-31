// SPDX-License-Identifier: AGPL-3.0-or-later

// Package state keeps project IDs and remembered ownership in state.json.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	FileName      = "state.json"
	SchemaVersion = 1
)

const (
	SourceVolume = "volume"
	SourceConfig = "config"
)

type Entry struct {
	Source string `json:"source"`
	// Name is the volume name for volumes.
	Name string `json:"name,omitempty"`
	// Path is the host path.
	Path string `json:"path"`
	// Pattern is the configured path or glob that produced the entry.
	Pattern    string            `json:"pattern,omitempty"`
	BindVolume bool              `json:"bind_volume,omitempty"`
	ProjectID  uint32            `json:"project_id"`
	WalkDone   bool              `json:"walk_done"`
	Owner      string            `json:"owner,omitempty"`
	Stack      string            `json:"stack,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	// ContainerLabels are the labels the owner container contributed, kept
	// while no owner container exists.
	ContainerLabels map[string]string `json:"container_labels,omitempty"`
	WorkingDir      string            `json:"working_dir,omitempty"`
	LastUsed        int64             `json:"last_used,omitempty"`
	// PendingReset marks an entry whose ID is reset to ResetTo before release.
	PendingReset bool   `json:"pending_reset,omitempty"`
	ResetTo      uint32 `json:"reset_to,omitempty"`
}

type State struct {
	SchemaVersion int               `json:"schema_version"`
	Entries       map[string]*Entry `json:"entries"`
	// DockerRoot is the last DockerRootDir seen, so the exclusion works
	// before Docker is reached after a restart.
	DockerRoot string `json:"docker_root,omitempty"`
}

func New() *State {
	return &State{SchemaVersion: SchemaVersion, Entries: map[string]*Entry{}}
}

func VolumeKey(name string) string { return "volume:" + name }
func ConfigKey(path string) string { return "config:" + path }

// ResetKey holds a pending reset whose original key was taken by another path.
func ResetKey(path string) string { return "reset:" + path }

// Load reads the state from dir. A missing file gives an empty state. An
// unreadable or newer file is moved to state.json.bak; the returned error says
// so and the returned state is empty.
func Load(dir string) (*State, error) {
	file := filepath.Join(dir, FileName)
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return New(), err
	}
	s := New()
	perr := json.Unmarshal(data, s)
	if perr == nil && s.SchemaVersion > SchemaVersion {
		perr = fmt.Errorf("schema_version %d is newer than %d", s.SchemaVersion, SchemaVersion)
	}
	if perr == nil && s.SchemaVersion < 1 {
		perr = fmt.Errorf("invalid schema_version %d", s.SchemaVersion)
	}
	if perr != nil {
		if rerr := os.Rename(file, file+".bak"); rerr != nil {
			return New(), fmt.Errorf("state unreadable (%v), and moving it failed: %w", perr, rerr)
		}
		return New(), fmt.Errorf("state unreadable, moved to %s.bak: %w", FileName, perr)
	}
	migrate(s)
	return s, nil
}

func migrate(s *State) {
	if s.Entries == nil {
		s.Entries = map[string]*Entry{}
	}
	// A null entry (hand edit) would panic on first use.
	for k, e := range s.Entries {
		if e == nil {
			delete(s.Entries, k)
		}
	}
	s.SchemaVersion = SchemaVersion
}

// Replaced in tests.
var (
	marshal    = json.MarshalIndent
	createTemp = func(dir, pattern string) (tempFile, error) { return os.CreateTemp(dir, pattern) }
)

type tempFile interface {
	Write(b []byte) (int, error)
	Sync() error
	Close() error
	Name() string
}

// Save writes the state atomically.
func (s *State) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := marshal(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := createTemp(dir, FileName+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, FileName))
}

// HoldsID reports whether any entry other than key holds id.
func (s *State) HoldsID(id uint32, exceptKey string) bool {
	for k, e := range s.Entries {
		if k != exceptKey && e.ProjectID == id {
			return true
		}
	}
	return false
}

// UsedIDs returns all IDs in the state.
func (s *State) UsedIDs() map[uint32]bool {
	m := map[uint32]bool{}
	for _, e := range s.Entries {
		if e.ProjectID != 0 {
			m[e.ProjectID] = true
		}
	}
	return m
}
