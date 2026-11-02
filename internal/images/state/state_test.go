// SPDX-License-Identifier: AGPL-3.0-or-later

package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil || len(s.Refs) != 0 || s.Since == nil || s.Registries == nil {
		t.Fatalf("empty: %v %v", s, err)
	}
	s.Refs["docker.io/library/nginx:1"] = &Ref{Digest: "sha256:a", LastSuccess: 5, Due: 9, Checked: true}
	s.Since[SinceKey("web", "docker.io/library/nginx:1")] = &Since{Since: 7}
	s.Registries["docker.io"] = &Registry{Until: 3, Step: 1}
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	r, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e := r.Refs["docker.io/library/nginx:1"]; e == nil || e.Digest != "sha256:a" || !e.Checked || e.Due != 9 {
		t.Fatalf("refs: %+v", r.Refs)
	}
	if e := r.Since["web|docker.io/library/nginx:1"]; e == nil || e.Since != 7 {
		t.Fatalf("since: %+v", r.Since)
	}
	if e := r.Registries["docker.io"]; e == nil || e.Step != 1 {
		t.Fatalf("registries: %+v", r.Registries)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("temp files left: %v", matches)
	}
}

func TestCorruptAndNewer(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt": "{not json",
		"newer":   `{"schema_version": 99}`,
		"zero":    `{"schema_version": 0}`,
	} {
		dir := t.TempDir()
		f := filepath.Join(dir, FileName)
		if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Load(dir)
		if err == nil || len(s.Refs) != 0 {
			t.Errorf("%s: expected error and empty state", name)
		}
		if _, err := os.Stat(f + ".bak"); err != nil {
			t.Errorf("%s: no .bak: %v", name, err)
		}
	}
}

func TestNullEntries(t *testing.T) {
	dir := t.TempDir()
	data := `{"schema_version": 1, "refs": {"a": null, "b": {"due": 1}}, "since": {"x": null},
		"registries": {"r": null}}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil || len(s.Refs) != 1 || len(s.Since) != 0 || len(s.Registries) != 0 {
		t.Fatalf("null entries: %+v %v", s, err)
	}
}

func TestLoadUnreadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "moved") {
		t.Fatalf("expected the unreadable state to be moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName+".bak")); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptNotMovable(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, FileName)
	if err := os.WriteFile(f, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory in the way makes the rename fail, even for root.
	if err := os.MkdirAll(filepath.Join(f+".bak", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "moving it failed") {
		t.Fatalf("got %v", err)
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

func TestSaveParentIsFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := New().Save(filepath.Join(file, "dir")); err == nil {
		t.Fatal("parent is a file")
	}
}

type failFile struct {
	name string
	fail string
}

func (f *failFile) Write(b []byte) (int, error) { return len(b), f.err("write") }
func (f *failFile) Sync() error                 { return f.err("sync") }
func (f *failFile) Close() error                { return f.err("close") }
func (f *failFile) Name() string                { return f.name }

func (f *failFile) err(step string) error {
	if f.fail == step {
		return errors.New(step + " failed")
	}
	return nil
}

func TestSaveWriteErrors(t *testing.T) {
	dir := t.TempDir()
	oldM, oldC := marshal, createTemp
	t.Cleanup(func() { marshal, createTemp = oldM, oldC })

	marshal = func(any, string, string) ([]byte, error) { return nil, errors.New("marshal failed") }
	if err := New().Save(dir); err == nil {
		t.Error("marshal")
	}
	marshal = oldM
	createTemp = func(string, string) (tempFile, error) { return nil, errors.New("create failed") }
	if err := New().Save(dir); err == nil {
		t.Error("create")
	}
	for _, step := range []string{"write", "sync", "close"} {
		createTemp = func(dir, _ string) (tempFile, error) {
			return &failFile{name: filepath.Join(dir, "tmp"), fail: step}, nil
		}
		if err := New().Save(dir); err == nil || !strings.Contains(err.Error(), step) {
			t.Errorf("%s: %v", step, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err == nil {
		t.Fatal("state must not be written")
	}
}
