// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motwok/DirAdvisor/internal/config"
	"github.com/motwok/DirAdvisor/internal/core"
	"github.com/motwok/DirAdvisor/internal/host"
)

func TestParseFlags(t *testing.T) {
	f, err := ParseFlags(nil, io.Discard)
	if err != nil || f.ConfigFile != "/etc/diradvisor/config.yml" || f.StoragePath != "/var/lib/diradvisor" ||
		f.RootFS != "/" || f.Listen != ":8080" || f.LogLevel != "info" || f.LogFormat != "logfmt" {
		t.Fatalf("defaults: %+v %v", f, err)
	}
	f, err = ParseFlags([]string{"--config.file=/data/c.yml", "--path.rootfs=/host", "--log.format=json", "--log.level=debug"}, io.Discard)
	if err != nil || f.ConfigFile != "/data/c.yml" || f.RootFS != "/host" || f.LogFormat != "json" {
		t.Fatalf("set: %+v %v", f, err)
	}
	for _, bad := range [][]string{{"--nope"}, {"--log.level=loud"}, {"--log.format=xml"}, {"extra"}} {
		if _, err := ParseFlags(bad, io.Discard); err == nil {
			t.Errorf("%v: expected error", bad)
		}
	}
}

func TestLogger(t *testing.T) {
	var b bytes.Buffer
	NewLogger(Flags{LogLevel: "warn", LogFormat: "json"}, &b).Warn("x", "k", 1)
	if !strings.Contains(b.String(), `"msg":"x"`) {
		t.Fatalf("json: %s", b.String())
	}
	b.Reset()
	NewLogger(Flags{LogLevel: "error", LogFormat: "logfmt"}, &b).Warn("hidden")
	if b.Len() != 0 {
		t.Fatal("level not applied")
	}
	if !strings.Contains(VersionString(), "diradvisor dev") {
		t.Fatal(VersionString())
	}
}

func newServer(t *testing.T, cfgText string) *Server {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yml")
	if cfgText != "" {
		if err := os.WriteFile(file, []byte(cfgText), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fs := host.New("/")
	cfg := config.Default()
	cfg.Docker.Enabled = false
	s := &Server{Flags: Flags{ConfigFile: file}, Log: log, FS: fs, reload: make(chan chan error), trigger: make(chan struct{}, 1)}
	s.Core = core.New(cfg, core.Options{FS: fs, StorageDir: dir, Log: log})
	return s
}

func get(t *testing.T, h http.Handler, method, p string) (int, string) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, p, nil))
	return w.Code, w.Body.String()
}

func TestHandler(t *testing.T) {
	s := newServer(t, "docker: { enabled: false }")
	h := s.Handler()
	if code, _ := get(t, h, "GET", "/-/healthy"); code != 200 {
		t.Fatal("healthy")
	}
	if code, _ := get(t, h, "GET", "/-/ready"); code != 503 {
		t.Fatal("ready before reconcile")
	}
	if err := s.Core.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, h, "GET", "/-/ready"); code != 200 {
		t.Fatal("ready after reconcile")
	}
	code, body := get(t, h, "GET", "/metrics")
	if code != 200 || !strings.Contains(body, "diradvisor_build_info") || !strings.Contains(body, "diradvisor_reconcile_errors_total 0") {
		t.Fatalf("metrics %d:\n%s", code, body)
	}
	if code, _ := get(t, h, "GET", "/-/reload"); code != 405 {
		t.Fatal("reload GET")
	}
	go func() {
		res := <-s.reload
		res <- s.Reload()
		res2 := <-s.reload
		os.WriteFile(s.Flags.ConfigFile, []byte("bad: ["), 0o644)
		res2 <- s.Reload()
	}()
	if code, body := get(t, h, "POST", "/-/reload"); code != 200 {
		t.Fatalf("reload: %d %s", code, body)
	}
	if code, _ := get(t, h, "POST", "/-/reload"); code != 500 {
		t.Fatal("bad reload must fail")
	}
	_, body = get(t, h, "GET", "/metrics")
	if !strings.Contains(body, "diradvisor_config_last_reload_successful 0") {
		t.Fatal("reload failure not visible")
	}
}

func TestMainExitCodes(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"--version"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "diradvisor") {
		t.Fatalf("version: %d %s", code, out.String())
	}
	if code := Main([]string{"--bogus"}, &out, &errb); code != 2 {
		t.Fatalf("bad flag: %d", code)
	}
	if code := Main([]string{"-h"}, &out, &errb); code != 0 {
		t.Fatalf("help: %d", code)
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "c.yml")
	os.WriteFile(bad, []byte("nope: 1"), 0o644)
	if code := Main([]string{"--config.file=" + bad, "--storage.path=" + dir}, &out, &errb); code != 1 {
		t.Fatalf("bad config: %d", code)
	}
	if code := Main([]string{"--config.file=" + filepath.Join(dir, "none"), "--storage.path=" + dir,
		"--web.listen-address=256.0.0.1:1", "--docker.socket="}, &out, &errb); code != 1 {
		t.Fatalf("listen failure: %d", code)
	}
}

func TestPoke(t *testing.T) {
	s := newServer(t, "")
	s.poke()
	s.poke()
	if len(s.trigger) != 1 {
		t.Fatal("trigger must hold one")
	}
}
