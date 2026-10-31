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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/motwok/DirAdvisor/internal/config"
	"github.com/motwok/DirAdvisor/internal/core"
	"github.com/motwok/DirAdvisor/internal/docker"
)

type handlerRT struct{ h http.Handler }

func (r handlerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w.Result(), nil
}

func TestLoopEvents(t *testing.T) {
	s := newServer(t, "docker: { enabled: false }\nglobal: { reconcile_interval: 5ms }")
	ctx, cancel := context.WithCancel(context.Background())
	hup := make(chan os.Signal, 1)
	done := make(chan int)
	go func() { done <- s.loop(ctx, hup) }()
	s.poke()
	hup <- syscall.SIGHUP
	res := make(chan error, 1)
	s.reload <- res
	if err := <-res; err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestLoopDockerTooOld(t *testing.T) {
	s := newServer(t, "")
	dc := docker.NewWithHTTP(handlerRT{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ApiVersion":"1.40"}`)
	})})
	s.Core = core.New(config.Default(), core.Options{FS: s.FS, Docker: dc, StorageDir: t.TempDir(), Log: s.Log})
	if code := s.loop(context.Background(), nil); code != 1 {
		t.Fatalf("exit %d", code)
	}
}

func TestReloadRequestCanceled(t *testing.T) {
	s := newServer(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/-/reload", nil).WithContext(ctx))
	if strings.Contains(w.Body.String(), "reloaded") {
		t.Fatal("no reload expected")
	}
}

func TestWatchEventsRetries(t *testing.T) {
	s := newServer(t, "")
	calls := 0
	dc := docker.NewWithHTTP(handlerRT{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		io.WriteString(w, `{"Type":"volume","Action":"create"}`+"\n")
	})})
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	s.watchEvents(ctx, dc)
	if calls != 2 || len(s.trigger) != 1 {
		t.Fatalf("calls %d, trigger %d", calls, len(s.trigger))
	}
}

func TestWatchEventsCanceledInStream(t *testing.T) {
	s := newServer(t, "")
	dc := docker.NewWithHTTP(handlerRT{http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	s.watchEvents(ctx, dc)
}

func TestMainDockerTooOld(t *testing.T) {
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
	os.WriteFile(cfg, []byte("directories: [ { path: /srv } ]\n"), 0o644)

	var out, errb bytes.Buffer
	code := Main([]string{"--config.file=" + cfg, "--storage.path=" + dir, "--path.rootfs=" + root,
		"--docker.socket=" + sock, "--web.listen-address=127.0.0.1:0"}, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "unsupported Docker") || !strings.Contains(errb.String(), "ZFS versions") {
		t.Fatalf("exit %d:\n%s", code, errb.String())
	}
}
