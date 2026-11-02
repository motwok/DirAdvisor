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

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/core"
	"github.com/motwok/HatchLens/internal/host"
)

func TestParseFlags(t *testing.T) {
	if p, err := ParseFlags(nil, io.Discard); err != nil || p != "" {
		t.Fatalf("none: %q %v", p, err)
	}
	if p, err := ParseFlags([]string{"-config", "/data/c.yml"}, io.Discard); err != nil || p != "/data/c.yml" {
		t.Fatalf("set: %q %v", p, err)
	}
	for _, bad := range [][]string{{"--nope"}, {"--version"}, {"extra"}} {
		if _, err := ParseFlags(bad, io.Discard); err == nil {
			t.Errorf("%v: expected error", bad)
		}
	}
}

func TestLogger(t *testing.T) {
	var b bytes.Buffer
	NewLogger("warn", "json", &b).Warn("x", "k", 1)
	if !strings.Contains(b.String(), `"msg":"x"`) {
		t.Fatalf("json: %s", b.String())
	}
	b.Reset()
	NewLogger("error", "logfmt", &b).Warn("hidden")
	if b.Len() != 0 {
		t.Fatal("level not applied")
	}
}

func newServer(t *testing.T, cfgText string) *Server {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fs := host.New("/")
	cfg, err := config.Parse([]byte(cfgText), os.LookupEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Docker.Enabled = false
	s := &Server{Log: log, FS: fs, trigger: make(chan struct{}, 1)}
	s.Core = core.New(cfg, core.Options{FS: fs, StorageDir: t.TempDir(), Log: log})
	return s
}

// A cancelled context stops the loop before the first reconcile.
func TestLoopStopsOnCancelledContext(t *testing.T) {
	s := newServer(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := s.loop(ctx); code != 0 || s.Core.Ready() {
		t.Fatalf("code %d ready %v", code, s.Core.Ready())
	}
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
	if code != 200 || !strings.Contains(body, "hatchlens_build_info") || !strings.Contains(body, "hatchlens_reconcile_errors_total 0") {
		t.Fatalf("metrics %d:\n%s", code, body)
	}
	if code, _ := get(t, h, "POST", "/-/reload"); code != 404 {
		t.Fatal("no reload endpoint")
	}
}

func metricsOf(t *testing.T, s *Server) string {
	t.Helper()
	_, body := get(t, s.Handler(), "GET", "/metrics")
	return body
}

func TestMainExitCodes(t *testing.T) {
	var errb bytes.Buffer
	if code := Main([]string{"--bogus"}, &errb); code != 2 {
		t.Fatalf("bad flag: %d", code)
	}
	if code := Main([]string{"-h"}, &errb); code != 0 {
		t.Fatalf("help: %d", code)
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "c.yml")
	os.WriteFile(bad, []byte("nope: 1"), 0o644)
	if code := Main([]string{"-config", bad}, &errb); code != 1 {
		t.Fatalf("bad config: %d", code)
	}
	if code := Main([]string{"-config", filepath.Join(dir, "none")}, &errb); code != 1 {
		t.Fatalf("missing file: %d", code)
	}
	file := filepath.Join(dir, "file")
	os.WriteFile(file, nil, 0o644)
	t.Setenv("HATCHLENS_CONFIG", "{ global: { rootfs: / }, directories: [ { path: '"+file+"' } ] }")
	if code := Main(nil, &errb); code != 1 || !strings.Contains(errb.String(), "not a directory") {
		t.Fatalf("not a directory: %d", code)
	}
	t.Setenv("HATCHLENS_CONFIG", "{ global: { listen_address: '256.0.0.1:1', storage_path: '"+dir+"' }, docker: { socket: '' } }")
	if code := Main(nil, &errb); code != 1 || !strings.Contains(errb.String(), "listen failed") {
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
