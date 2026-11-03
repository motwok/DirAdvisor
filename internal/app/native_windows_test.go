// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows/svc"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/winhost"
)

type nullBackend struct{ winhost.Backend }

func restoreNative(t *testing.T) {
	t.Cleanup(func() {
		isService, runService, startHost, dataDir, openLog = svc.IsWindowsService, svc.Run, winhost.Start, winhost.DataDir, winhost.OpenLog
	})
}

// quietConfig runs nothing but the HTTP server.
func quietConfig(t *testing.T) string {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.yml")
	os.WriteFile(cfg, []byte(`{ disable: [ directory, volume, image, cpu, ram, disk, net ], docker_socket: "", `+
		`listen_address: "127.0.0.1:0", storage_path: '`+dir+`' }`), 0o644)
	return cfg
}

func TestAsService(t *testing.T) {
	restoreNative(t)
	isService = func() (bool, error) { return false, nil }
	if _, ok := asService(nil); ok {
		t.Fatal("not a service")
	}
	isService = func() (bool, error) { return false, errors.New("x") }
	if _, ok := asService(nil); ok {
		t.Fatal("unknown means not a service")
	}
	dir := t.TempDir()
	isService = func() (bool, error) { return true, nil }
	openLog = func(name string) (*os.File, error) { return os.Create(filepath.Join(dir, name)) }
	runService = func(string, svc.Handler) error { return nil }
	if code, ok := asService(nil); !ok || code != 0 {
		t.Fatal("service")
	}
	runService = func(string, svc.Handler) error { return errors.New("x") }
	if code, _ := asService(nil); code != 1 {
		t.Fatal("service error")
	}
	openLog = func(string) (*os.File, error) { return nil, errors.New("x") }
	if code, _ := asService(nil); code != 1 {
		t.Fatal("log error")
	}
	if Main([]string{"--bogus"}, io.Discard) != 1 {
		t.Fatal("Main runs as the service")
	}
}

func TestServiceExecute(t *testing.T) {
	restoreNative(t)
	startHost = func(context.Context, *slog.Logger) winhost.Backend { return nullBackend{} }
	req := make(chan svc.ChangeRequest)
	st := make(chan svc.Status, 10)
	res := make(chan uint32)
	go func() {
		_, code := handler{args: []string{"-config", quietConfig(t)}, w: io.Discard}.Execute(nil, req, st)
		res <- code
	}()
	req <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: svc.Status{State: svc.Running}}
	req <- svc.ChangeRequest{Cmd: svc.Stop}
	if code := <-res; code != 0 {
		t.Fatalf("stop: %d", code)
	}
	if _, code := (handler{args: []string{"--bogus"}, w: io.Discard}).Execute(nil, make(chan svc.ChangeRequest), make(chan svc.Status, 10)); code != 2 {
		t.Fatalf("bad flag: %d", code)
	}
}

func TestNativeDefaults(t *testing.T) {
	restoreNative(t)
	dir := t.TempDir()
	dataDir = func() string { return dir }
	t.Setenv("HATCHLENS_CONFIG_FILE", "")
	t.Setenv("HATCHLENS_CONFIG", "")
	env := configEnv(true)
	if env("HATCHLENS_CONFIG_FILE") != "" {
		t.Fatal("no file yet")
	}
	os.WriteFile(filepath.Join(dir, "config.yml"), nil, 0o644)
	if env("HATCHLENS_CONFIG_FILE") != filepath.Join(dir, "config.yml") || env("X") != "" {
		t.Fatal("DataDir config")
	}
	t.Setenv("HATCHLENS_CONFIG", "x: 1")
	if env("HATCHLENS_CONFIG_FILE") != "" {
		t.Fatal("inline config wins")
	}
	if configEnv(false)("HATCHLENS_CONFIG") != "x: 1" {
		t.Fatal("plain environment")
	}
	cfg := config.Default()
	nativeStorage(cfg, false)
	if cfg.Global.StoragePath != `C:\data` {
		t.Fatal("not native")
	}
	nativeStorage(cfg, true)
	if cfg.Global.StoragePath != dir {
		t.Fatal("native")
	}
	cfg.Global.StoragePath = `D:\state`
	nativeStorage(cfg, true)
	if cfg.Global.StoragePath != `D:\state` {
		t.Fatal("a set path stays")
	}
	startHost = func(context.Context, *slog.Logger) winhost.Backend { return nullBackend{} }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if hostService(context.Background(), cfg, log, true) == nil || hostService(context.Background(), cfg, log, false) == nil {
		t.Fatal("host service")
	}
	cfg.HostSocket = ""
	if hostService(context.Background(), cfg, log, false) != nil {
		t.Fatal("no socket, no service")
	}
}
