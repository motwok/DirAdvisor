// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/core"
	"github.com/motwok/HatchLens/internal/docker"
)

type handlerRT struct{ h http.Handler }

func (r handlerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w.Result(), nil
}

func TestLoopEvents(t *testing.T) {
	s := testServer(t, "disable: [ volume ]\nreconcile_interval: 5ms")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- s.loop(ctx) }()
	s.poke()
	time.Sleep(30 * time.Millisecond)
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestLoopDockerTooOld(t *testing.T) {
	s := testServer(t, "")
	dc := docker.NewWithHTTP(handlerRT{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ApiVersion":"1.40"}`)
	})})
	s.Core = core.New(config.Default(), core.Options{FS: s.FS, Docker: dc, StorageDir: t.TempDir(), Log: s.Log})
	if code := s.loop(context.Background()); code != 1 {
		t.Fatalf("exit %d", code)
	}
}

func TestWatchEventsRetries(t *testing.T) {
	s := testServer(t, "")
	calls := 0
	dc := docker.NewWithHTTP(handlerRT{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		io.WriteString(w, `{"Type":"volume","Action":"create"}`+"\n")
	})})
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	s.watchEvents(ctx, dc, time.Second, time.Minute)
	if calls != 2 || len(s.trigger) != 1 {
		t.Fatalf("calls %d, trigger %d", calls, len(s.trigger))
	}
}

func TestWatchEventsCanceledInStream(t *testing.T) {
	s := testServer(t, "")
	dc := docker.NewWithHTTP(handlerRT{http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	s.watchEvents(ctx, dc, time.Second, time.Minute)
}

func TestMainDockerTooOld(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Docker is a named pipe on Windows")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ApiVersion":"1.40"}`)
	}))
	srv.Listener = l
	srv.Start()
	defer srv.Close()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sys/module/zfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "sys/module/zfs/version"), []byte("2.2.6\n"), 0o644)
	cfg := filepath.Join(dir, "c.yml")
	os.WriteFile(cfg, []byte("listen_address: '127.0.0.1:0'\nstorage_path: '"+dir+"'\nrootfs: '"+root+"'\n"+
		"docker_socket: '"+sock+"'\ndirectories: [ { path: /srv } ]\n"), 0o644)

	var errb bytes.Buffer
	code := Main([]string{"-config", cfg}, &errb)
	if code != 1 || !strings.Contains(errb.String(), "unsupported Docker") || !strings.Contains(errb.String(), "ZFS versions") {
		t.Fatalf("exit %d:\n%s", code, errb.String())
	}
}

func TestWatchEventsBackoff(t *testing.T) {
	s := testServer(t, "")
	calls := 0
	dc := docker.NewWithHTTP(handlerRT{http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		// Every second stream lasts longer than the maximum and resets the backoff.
		if calls++; calls%2 == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	})})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s.watchEvents(ctx, dc, time.Millisecond, 3*time.Millisecond)
	if calls < 4 {
		t.Fatalf("calls %d", calls)
	}
}

func TestWaitDoneTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waitDone(ctx, nil)
}
