// SPDX-License-Identifier: AGPL-3.0-or-later

package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil || len(s.Entries) != 0 {
		t.Fatalf("empty: %v %v", s, err)
	}
	s.Entries[VolumeKey("v")] = &Entry{Source: SourceVolume, Name: "v", Path: "/p", ProjectID: 100000, WalkDone: true}
	s.Entries[ConfigKey("/srv")] = &Entry{Source: SourceConfig, Path: "/srv", ProjectID: 100001}
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	r, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e := r.Entries["volume:v"]; e == nil || e.ProjectID != 100000 || !e.WalkDone {
		t.Fatalf("roundtrip: %+v", r.Entries)
	}
	if !r.HoldsID(100001, "volume:v") || r.HoldsID(100000, "volume:v") || len(r.UsedIDs()) != 2 {
		t.Fatal("HoldsID/UsedIDs")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("temp files left: %v", matches)
	}
}

func TestCorruptAndNewer(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt": "{not json",
		"newer":   `{"schema_version": 99, "entries": {}}`,
		"zero":    `{"schema_version": 0}`,
	} {
		dir := t.TempDir()
		f := filepath.Join(dir, FileName)
		if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Load(dir)
		if err == nil || len(s.Entries) != 0 {
			t.Errorf("%s: expected error and empty state", name)
		}
		if _, err := os.Stat(f + ".bak"); err != nil {
			t.Errorf("%s: no .bak: %v", name, err)
		}
	}
}

func TestMigrateNilEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"schema_version": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil || s.Entries == nil {
		t.Fatalf("migrate: %v %v", s, err)
	}
}

func TestLoadUnreadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for a directory")
	}
}

func TestSaveCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := New().Save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
}
