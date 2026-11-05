// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/motwok/HatchLens/internal/docker"
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
		!c.Enabled(FnImage) || c.Images.CheckInterval.D() != 24*time.Hour || c.Images.ForgetAfter.D() != 168*time.Hour ||
		c.Global.ProjectIDs != (IDRange{100000, 199999}) || !c.Runs(FnVolume) || !c.Runs(FnDirectory) || c.HostSocket != defaultHostSocket ||
		c.Global.ListenAddress != ":8080" || c.Global.LogLevel != "info" || c.Global.LogFormat != "logfmt" ||
		c.Global.StoragePath != defaultStorage || c.Global.RootFS != "/host" || c.DockerSocket != docker.DefaultSocket {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestFullConfig(t *testing.T) {
	skipOnWindows(t)
	data := `
disable: [ volume, image ]
reconcile_interval: 30s
walk_files_per_second: 100
project_ids: { from: 5000, to: 6000 }
exclude:
  volumes: [ "buildx_*" ]
  containers: [ "buildx_*" ]
  images: [ "${REG}/*" ]
container_labels:
  - com.example.team
  - { from: com.example.backup, to: backup }
volume_labels: [ com.example.retention ]
labels:
  - match: { stack: mailcow }
    labels: { team: mail }
  - match: { stack: "mail*", container: c, image: i, volume: v, path: /p }
    labels: { team: "${TEAM:-mail}" }
directories:
  - path: ${ROOT}/backup
    labels: { stack: backup, owner: borg }
  - path: /home/*
    labels: { user: "$1" }
images:
  check_interval: 6h
  forget_after: 48h
  registries:
    - host: ghcr.io
      username: ${U}
      password: ${P}
    - { host: index.docker.io, username: x, password: "${HUB:-}" }
`
	c, err := Parse([]byte(data), env(map[string]string{"ROOT": "/srv", "REG": "ghcr.io/x", "U": "u", "P": "p"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Global.ReconcileInterval.D() != 30*time.Second || c.Global.WalkFilesPerSecond != 100 ||
		c.Enabled(FnVolume) || !c.Enabled(FnDirectory) {
		t.Fatalf("settings/disable not read: %+v", c)
	}
	if got := c.ContainerLabels[0].Target("container_label_"); got != "container_label_com_example_team" {
		t.Fatalf("target %q", got)
	}
	if got := c.ContainerLabels[1].Target("container_label_"); got != "backup" {
		t.Fatalf("target %q", got)
	}
	if c.Directories[0].Path != "/srv/backup" || c.Directories[1].Labels["user"] != "$1" {
		t.Fatalf("directories: %+v", c.Directories)
	}
	im := c.Images
	if c.Enabled(FnImage) || im.CheckInterval.D() != 6*time.Hour || c.Exclude.Images[0] != "ghcr.io/x/*" ||
		c.Exclude.Containers[0] != "buildx_*" || c.Exclude.Volumes[0] != "buildx_*" {
		t.Fatalf("images: %+v", im)
	}
	if r, ok := im.Registry("ghcr.io"); !ok || r.Username != "u" || r.Password != "p" {
		t.Fatalf("ghcr: %+v", r)
	}
	if r, ok := im.Registry("docker.io"); !ok || r.Password != "" {
		t.Fatalf("hub: %+v", r)
	}
	if _, ok := im.Registry("quay.io"); ok {
		t.Fatal("quay")
	}
	if r := c.Labels[1]; r.Match.Stack != "mail*" || r.Match.Container != "c" || r.Match.Image != "i" ||
		r.Match.Volume != "v" || r.Match.Path != "/p" || r.Labels["team"] != "mail" {
		t.Fatalf("%+v", r)
	}
}

func TestInvalidImages(t *testing.T) {
	for _, data := range []string{
		"images: { check_interval: 0s }",
		"images: { forget_after: -1h }",
		"images: { registries: [ { username: x } ] }",
		"images: { registries: [ { host: docker.io }, { host: index.docker.io } ] }",
		"container_labels: [ '' ]",
		"container_labels: [ { from: a, to: '1x' } ]",
		"container_labels: [ { from: a, to: stack } ]",
		"container_labels: [ { from: a, to: mode } ]",
		"container_labels: [ { from: a, to: path } ]",
		"volume_labels: [ { from: a, to: volume } ]",
		"container_labels: [ a.b, a_b ]",
		"labels: [ { labels: { mode: x } } ]",
		"labels: [ { labels: { path: x } } ]",
		"labels: [ { labels: { 'a-b': x } } ]",
		"docker: { socket: x }",
		"images: { exclude: { images: [ x ] } }",
		"images: { labels: [] }",
		"exclude: { images: [ '${MISSING}' ] }",
	} {
		if _, err := Parse([]byte(data), env(nil), nil); err == nil {
			t.Errorf("%q: expected error", data)
		}
	}
	// The error names the key, never the value.
	_, err := Parse([]byte(`images: { registries: [ { host: x, password: "pa${ss" } ] }`), env(nil), nil)
	if err == nil || !strings.HasPrefix(err.Error(), "images.registries[0].password: unterminated ${") {
		t.Fatalf("error: %v", err)
	}
}

func TestImageHelpers(t *testing.T) {
	if !IsImageOwn("mode") || IsImageOwn("team") {
		t.Fatal("IsImageOwn")
	}
	if !MatchAny([]string{"x", "a*"}, "b", "ab") || MatchAny([]string{"x"}, "y") || MatchAny(nil, "y") ||
		!MatchAny([]string{"ghcr.io/*"}, "ghcr.io/a/b:1") {
		t.Fatal("MatchAny")
	}
}

func TestHostSocket(t *testing.T) {
	c, err := Parse([]byte(`directories: [ { path: 'D:\data\x' } ]`), env(nil), nil)
	if err != nil || c.Directories[0].Path != "/D:/data/x" {
		t.Fatalf("Windows path: %+v %v", c, err)
	}
	if windows {
		if err := c.CheckHostSocket(); err != nil {
			t.Fatal(err)
		}
	} else if err := c.CheckHostSocket(); err == nil || !strings.Contains(err.Error(), `D:\data\x needs host_socket`) {
		t.Fatalf("a Windows path on Linux needs the host service: %v", err)
	}
	c, err = Parse([]byte("host_socket: tcp://host.docker.internal:8091\nhost_token: ${T}\ndirectories: [ { path: 'D:\\data' } ]"), env(map[string]string{"T": "tok"}), nil)
	if err != nil || c.HostToken != "tok" || c.Directories[0].Path != "/D:/data" {
		t.Fatalf("TCP: %+v %v", c, err)
	}
	if _, err := Parse([]byte(`host_socket: tcp://host.docker.internal:8091`), env(nil), nil); err == nil || !strings.Contains(err.Error(), "host_token") {
		t.Fatalf("TCP without token: %v", err)
	}
	c, err = Parse([]byte(`host_socket: ""`), env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckHostSocket(); windows != (err != nil) {
		t.Fatalf("empty host_socket on windows=%v: %v", windows, err)
	}
	c, err = Parse([]byte(`{ host_socket: "", disable: [ directory, volume ] }`), env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckHostSocket(); err != nil {
		t.Fatal(err)
	}
	if c.StorageSet() {
		t.Fatal("StorageSet without the key")
	}
	if c, err = Parse([]byte(`storage_path: /x`), env(nil), nil); err != nil || !c.StorageSet() {
		t.Fatalf("StorageSet: %v", err)
	}
}

func TestInvalid(t *testing.T) {
	cases := map[string]string{
		"unknown key":       "foo: 1",
		"log level":         "log_level: loud",
		"log format":        "log_format: xml",
		"unset var setting": "rootfs: \"${NOPE}\"",
		"old global":        "global: { reconcile_interval: 5s }",
		"zero duration":     "reconcile_interval: 0s",
		"bad duration":      "reconcile_interval: soon",
		"negative rate":     "walk_files_per_second: -1",
		"huge rate":         "walk_files_per_second: 2000000000",
		"range order":       "project_ids: { from: 10, to: 10 }",
		"range zero":        "project_ids: { from: 0, to: 10 }",
		"range max":         "project_ids: { from: 1, to: 4294967295 }",
		"unknown function":  "disable: [ gpu ]",
		"socket for cpu":    "{ disable: [ volume, image ], docker_socket: \"\" }",
		"rule own network":  "labels: [ { match: { stack: a }, labels: { network: x } } ]",
		"socket needed":     "docker_socket: \"\"",
		"socket for image":  "{ disable: [ volume ], docker_socket: \"\" }",
		"label name":        "directories: [ { path: /srv, labels: { 1x: a } } ]",
		"label dunder":      "directories: [ { path: /srv, labels: { __x: a } } ]",
		"own label":         "directories: [ { path: /srv, labels: { path: a } } ]",
		"own label rule":    "labels: [ { labels: { stack: a } } ]",
		"own label to":      "container_labels: [ { from: a, to: owner } ]",
		"dup target":        "container_labels: [ a.b, a_b ]",
		"ref unknown key":   "container_labels: [ { name: a } ]",
		"relative":          "directories: [ { path: srv } ]",
		"double star":       "directories: [ { path: /srv/** } ]",
		"root":              "directories: [ { path: / } ]",
		"excluded":          "directories: [ { path: /var/lib/docker/volumes } ]",
		"excluded glob":     "directories: [ { path: /proc/* } ]",
		"unset var":         "directories: [ { path: \"${NOPE}\" } ]",
		"unset vars":        "directories: [ { path: \"${NOPE}\" }, { path: \"${NOPE2}\" } ]",
		"unterminated var":  "directories: [ { path: \"${NOPE\" } ]",
		"ref not a string":  "container_labels: [ { from: [a] } ]",
		"ref sequence":      "container_labels: [ [a] ]",
		"empty from":        "volume_labels: [ \"\" ]",
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data), env(nil), nil); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestAllowed(t *testing.T) {
	skipOnWindows(t)
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
	skipOnWindows(t)
	stat := func(p string) (bool, bool) { return p == "/srv/file", false }
	if _, err := Parse([]byte("directories: [ { path: /srv/file } ]"), env(nil), stat); err == nil {
		t.Fatal("expected error for a file")
	}
	if _, err := Parse([]byte("directories: [ { path: /srv/* } ]"), env(nil), stat); err != nil {
		t.Fatalf("globs are not checked: %v", err)
	}
	if _, err := Parse([]byte("directories: [ { path: /srv/missing } ]"), env(nil), stat); err != nil {
		t.Fatalf("missing path must be allowed: %v", err)
	}
}

func TestSource(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.yml")
	if err := os.WriteFile(f, []byte("reconcile_interval: 5s"), 0o644); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	getenv := func(n string) string { return vars[n] }
	if data, src, err := Source("", getenv); err != nil || src != "defaults" || len(data) != 0 {
		t.Fatalf("none: %q %q %v", data, src, err)
	}
	vars["HATCHLENS_CONFIG"] = "inline"
	if data, src, err := Source("", getenv); err != nil || src != "HATCHLENS_CONFIG" || string(data) != "inline" {
		t.Fatalf("inline: %q %q %v", data, src, err)
	}
	vars["HATCHLENS_CONFIG_FILE"] = f
	if data, src, err := Source("", getenv); err != nil || src != f || !strings.Contains(string(data), "5s") {
		t.Fatalf("file from env: %q %q %v", data, src, err)
	}
	none := filepath.Join(dir, "none.yml")
	if _, src, err := Source(none, getenv); err == nil || src != none {
		t.Fatalf("missing flag file: %q %v", src, err)
	}
	vars["HATCHLENS_CONFIG_FILE"] = none
	if _, _, err := Source("", getenv); err == nil {
		t.Fatal("missing env file must fail")
	}
}

func TestExpandSettings(t *testing.T) {
	data := "{ disable: [ volume, image, cpu, ram, disk, net ], rootfs: \"${R}\", docker_socket: \"\" }"
	c, err := Parse([]byte(data), env(map[string]string{"R": "/"}), nil)
	if err != nil || c.Global.RootFS != "/" || c.DockerSocket != "" {
		t.Fatalf("%+v %v", c, err)
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

func skipOnWindows(t *testing.T) {
	if windows {
		t.Skip("no directories on Windows")
	}
}

func TestRules(t *testing.T) {
	c, err := Parse([]byte(`
labels:
  - match: { stack: app }
    labels: { a: "1" }
  - match: { volume: "v*" }
    labels: { b: "2" }
  - match: { image: "nginx*" }
    labels: { c: "3" }
  - match: { path: "/srv/*" }
    labels: { a: "4" }
  - match: { container: web }
    labels: { d: "5" }
  - match: { path: 'C:\data\*' }
    labels: { e: "6" }
`), env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	win := "/C:/data/x"
	if got := c.RuleLabels(Subject{Path: &win}); !reflect.DeepEqual(got, map[string]string{"e": "6"}) {
		t.Fatalf("Windows path rule: %v", got)
	}
	stack, vol, path, ctr := "app", "vol1", "/srv/x", "web"
	if got := c.RuleLabels(Subject{Stack: &stack, Volume: &vol, Path: &path, Container: &ctr}); !reflect.DeepEqual(got,
		map[string]string{"a": "4", "b": "2", "d": "5"}) {
		t.Fatalf("volume: %v", got)
	}
	if got := c.RuleLabels(Subject{Stack: &stack, Container: &ctr, Images: []string{"nginx:1"}}); !reflect.DeepEqual(got,
		map[string]string{"a": "1", "c": "3", "d": "5"}) {
		t.Fatalf("image: %v", got)
	}
	if got := c.RuleLabels(Subject{Stack: &stack, Path: &path}); !reflect.DeepEqual(got, map[string]string{"a": "4"}) {
		t.Fatalf("directory: %v", got)
	}
	if Opt("") != nil || *Opt("x") != "x" {
		t.Fatal("Opt")
	}
	any, err := Parse([]byte("labels: [ { match: { stack: '*' }, labels: { s: '1' } } ]"), env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := any.ExtraLabels("web", nil, "", nil, func(string) bool { return false }, nil); len(got) != 0 {
		t.Fatalf("no stack, no match (LBL-7): %v", got)
	}
	if got := any.ExtraLabels("web", nil, "app", nil, func(string) bool { return false }, nil); got["s"] != "1" {
		t.Fatalf("stack matches: %v", got)
	}
	labels := map[string]string{LabelEnable: "false"}
	if !c.ExcludedContainer("x", labels) || c.ExcludedContainer("x", nil) {
		t.Fatal("enable label")
	}
	c.Exclude = Exclude{Containers: []string{"db*"}, Images: []string{"docker.io/library/redis*"}}
	if !c.ExcludedContainer("db1", nil) || !c.ExcludedContainer("x", nil, "redis:7", "docker.io/library/redis:7") ||
		c.ExcludedContainer("x", nil, "nginx") {
		t.Fatal("globs")
	}
}
