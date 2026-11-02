// SPDX-License-Identifier: AGPL-3.0-or-later

// Package state keeps registry results and since times in images.json.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	FileName      = "images.json"
	SchemaVersion = 1
)

// Ref is what is known about one normalized reference.
type Ref struct {
	Digest      string `json:"digest,omitempty"`
	LastSuccess int64  `json:"last_success,omitempty"`
	Due         int64  `json:"due"`
	Fails       int    `json:"fails,omitempty"`
	// Reason of the last check if it failed, empty after a success.
	Reason string `json:"reason,omitempty"`
	// Checked is set after the first check, successful or not.
	Checked bool `json:"checked,omitempty"`
	// Unused is when the last container using the reference went away.
	Unused int64 `json:"unused,omitempty"`
}

// Since is the since time of one key and reference.
type Since struct {
	Since  int64 `json:"since"`
	Unused int64 `json:"unused,omitempty"`
}

// Registry is the rate limit backoff of one registry.
type Registry struct {
	Until int64 `json:"until"`
	Step  int   `json:"step"`
}

type State struct {
	SchemaVersion int                  `json:"schema_version"`
	Refs          map[string]*Ref      `json:"refs"`
	Since         map[string]*Since    `json:"since"`
	Registries    map[string]*Registry `json:"registries"`
}

func New() *State {
	s := &State{SchemaVersion: SchemaVersion}
	migrate(s)
	return s
}

// SinceKey is the key of the Since map.
func SinceKey(key, ref string) string { return key + "|" + ref }

// Load reads the state from dir. A missing file gives an empty state. An
// unreadable or newer file is moved to images.json.bak; the returned error says
// so and the returned state is empty.
func Load(dir string) (*State, error) {
	file := filepath.Join(dir, FileName)
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	s := &State{}
	perr := err
	if perr == nil {
		perr = json.Unmarshal(data, s)
	}
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
	if s.Refs == nil {
		s.Refs = map[string]*Ref{}
	}
	if s.Since == nil {
		s.Since = map[string]*Since{}
	}
	if s.Registries == nil {
		s.Registries = map[string]*Registry{}
	}
	// A null entry (hand edit) would panic on first use.
	for k, v := range s.Refs {
		if v == nil {
			delete(s.Refs, k)
		}
	}
	for k, v := range s.Since {
		if v == nil {
			delete(s.Since, k)
		}
	}
	for k, v := range s.Registries {
		if v == nil {
			delete(s.Registries, k)
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
