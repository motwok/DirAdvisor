// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) LookupFunc {
	return func(n string) (string, bool) { v, ok := m[n]; return v, ok }
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(""), env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Global.ReconcileInterval.D() != 60*time.Second || c.Global.AdminActionLogInterval.D() != time.Hour ||
		c.Global.ImageUpdateCheck.Interval.D() != 24*time.Hour || c.Global.ImageUpdateCheck.Enabled ||
		c.Global.ProjectIDs != (IDRange{100000, 199999}) || !c.Docker.Enabled {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestFullConfig(t *testing.T) {
	data := `
global:
  reconcile_interval: 30s
  walk_files_per_second: 100
  image_update_check: { enabled: true, interval: 12h }
  project_ids: { from: 5000, to: 6000 }
docker:
  enabled: false
  exclude:
    volumes: [ "buildx_*" ]
  container_labels:
    - com.example.team
    - { from: com.example.backup, to: backup }
  volume_labels: [ com.example.retention ]
  labels:
    - match: { stack: mailcow }
      labels: { team: mail }
directories:
  - path: ${ROOT}/backup
    labels: { stack: backup, owner: borg }
  - path: /home/*
    labels: { user: "$1" }
`
	c, err := Parse([]byte(data), env(map[string]string{"ROOT": "/srv"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Global.ReconcileInterval.D() != 30*time.Second || c.Global.WalkFilesPerSecond != 100 ||
		!c.Global.ImageUpdateCheck.Enabled || c.Docker.Enabled {
		t.Fatalf("global/docker not read: %+v", c)
	}
	if got := c.Docker.ContainerLabels[0].Target("container_label_"); got != "container_label_com_example_team" {
		t.Fatalf("target %q", got)
	}
	if got := c.Docker.ContainerLabels[1].Target("container_label_"); got != "backup" {
		t.Fatalf("target %q", got)
	}
	if c.Directories[0].Path != "/srv/backup" || c.Directories[1].Labels["user"] != "$1" {
		t.Fatalf("directories: %+v", c.Directories)
	}
}

func TestInvalid(t *testing.T) {
	cases := map[string]string{
		"unknown key":      "foo: 1",
		"unknown nested":   "global: { bar: 1 }",
		"zero duration":    "global: { reconcile_interval: 0s }",
		"bad duration":     "global: { reconcile_interval: soon }",
		"negative rate":    "global: { walk_files_per_second: -1 }",
		"range order":      "global: { project_ids: { from: 10, to: 10 } }",
		"range zero":       "global: { project_ids: { from: 0, to: 10 } }",
		"range max":        "global: { project_ids: { from: 1, to: 4294967295 } }",
		"label name":       "directories: [ { path: /srv, labels: { 1x: a } } ]",
		"label dunder":     "directories: [ { path: /srv, labels: { __x: a } } ]",
		"own label":        "directories: [ { path: /srv, labels: { path: a } } ]",
		"own label rule":   "docker: { labels: [ { labels: { stack: a } } ] }",
		"own label to":     "docker: { container_labels: [ { from: a, to: owner } ] }",
		"dup target":       "docker: { container_labels: [ a.b, a_b ] }",
		"ref unknown key":  "docker: { container_labels: [ { name: a } ] }",
		"relative":         "directories: [ { path: srv } ]",
		"double star":      "directories: [ { path: /srv/** } ]",
		"root":             "directories: [ { path: / } ]",
		"excluded":         "directories: [ { path: /var/lib/docker/volumes } ]",
		"excluded glob":    "directories: [ { path: /proc/* } ]",
		"unset var":        "directories: [ { path: \"${NOPE}\" } ]",
		"unset vars":       "directories: [ { path: \"${NOPE}\" }, { path: \"${NOPE2}\" } ]",
		"unterminated var": "directories: [ { path: \"${NOPE\" } ]",
		"ref not a string": "docker: { container_labels: [ { from: [a] } ] }",
		"ref sequence":     "docker: { container_labels: [ [a] ] }",
		"empty from":       "docker: { volume_labels: [ \"\" ] }",
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data), env(nil), nil); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestAllowed(t *testing.T) {
	ok := []string{
		"directories: [ { path: /srv, labels: { stack: a, owner: b } } ]",
		"directories: [ { path: /* } ]",
		"directories: [ { path: /var/lib/* } ]",
		"directories: [ { path: \"/srv/${X:-data}\" } ]",
	}
	for _, data := range ok {
		if _, err := Parse([]byte(data), env(nil), nil); err != nil {
			t.Errorf("%q: %v", data, err)
		}
	}
}

func TestNotADirectory(t *testing.T) {
	stat := func(p string) (bool, bool) { return p == "/srv/file", false }
	if _, err := Parse([]byte("directories: [ { path: /srv/file } ]"), env(nil), stat); err == nil {
		t.Fatal("expected error for a file")
	}
	if _, err := Parse([]byte("directories: [ { path: /srv/missing } ]"), env(nil), stat); err != nil {
		t.Fatalf("missing path must be allowed: %v", err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	c, found, err := Load(filepath.Join(dir, "none.yml"), env(nil), nil)
	if err != nil || found || c == nil {
		t.Fatalf("missing file: %v %v", found, err)
	}
	f := filepath.Join(dir, "c.yml")
	if err := os.WriteFile(f, []byte("global: { reconcile_interval: 5s }"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, found, err = Load(f, env(nil), nil)
	if err != nil || !found || c.Global.ReconcileInterval.D() != 5*time.Second {
		t.Fatalf("load: %v %v %+v", found, err, c)
	}
	if err := os.WriteFile(f, []byte("bad: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(f, env(nil), nil); err == nil {
		t.Fatal("expected parse error")
	}
	if _, _, err := Load(dir, env(nil), nil); err == nil {
		t.Fatal("expected read error for a directory")
	}
}

func TestExpand(t *testing.T) {
	e := env(map[string]string{"A": "x", "E": ""})
	cases := map[string]string{
		"plain":      "plain",
		"${A}":       "x",
		"a${A}b":     "axb",
		"${B:-def}":  "def",
		"${E:-def}":  "def",
		"${A:-def}":  "x",
		"$$":         "$",
		"$$A":        "$A",
		"$1":         "$1",
		"cost $":     "cost $",
		"$x":         "$x",
		"${A}${A}$1": "xx$1",
	}
	for in, want := range cases {
		got, err := Expand(in, e)
		if err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"${B}", "${", "${1A}"} {
		if _, err := Expand(in, e); err == nil {
			t.Errorf("Expand(%q): expected error", in)
		}
	}
}

func TestExcluded(t *testing.T) {
	yes := []string{"/", "/var/lib/docker", "/var/lib/docker/volumes/x", "/proc/1", "/run", "/srv/docker/x"}
	no := []string{"/srv", "/var/lib", "/var/lib/dockerx", "/home"}
	for _, p := range yes {
		if !Excluded(p, "/srv/docker") {
			t.Errorf("%s should be excluded", p)
		}
	}
	for _, p := range no {
		if Excluded(p, "/srv/docker") {
			t.Errorf("%s should not be excluded", p)
		}
	}
}

func TestSanitize(t *testing.T) {
	if got := SanitizeLabelName("com.docker-compose/x"); got != "com_docker_compose_x" {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(OwnLabels, []string{"path", "source", "volume", "stack", "owner", "state", "project_id"}) {
		t.Fatal("own labels changed")
	}
	if !strings.HasPrefix(LabelRef{From: "a"}.Target("p_"), "p_") {
		t.Fatal("target prefix")
	}
}
