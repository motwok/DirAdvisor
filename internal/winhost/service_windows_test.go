// SPDX-License-Identifier: AGPL-3.0-or-later

package winhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestHostBackend(t *testing.T) {
	v, base := needVolume(t)
	windows.CloseHandle(v.h)
	windows.CloseHandle(v.hint)
	h := newHost(quiet())
	write(t, filepath.Join(base, "a", "f"), 100000)
	os.MkdirAll(filepath.Join(base, "d"), 0o755)
	a := filepath.Join(base, "a")
	if !h.Stat(a).Dir || h.Stat(filepath.Join(base, "missing")).Exists || h.Stat(filepath.Join(base, "a", "f", "x")).Exists {
		t.Fatal("stat")
	}
	if !h.Stat("bad\x00name").Exists {
		t.Fatal("stat error is not gone")
	}
	if d, err := h.Dirs(base); err != nil || len(d) != 2 {
		t.Fatalf("dirs %v %v", d, err)
	}
	if _, err := h.Dirs(filepath.Join(base, "missing")); err == nil {
		t.Fatal("dirs missing")
	}
	m, err := h.Mount(a)
	if err != nil || m.FSType != "NTFS" || m.Status != "" {
		t.Fatalf("mount %+v %v", m, err)
	}
	if err := h.Assign(context.Background(), a, 5); err != nil {
		t.Fatal(err)
	}
	if id, err := h.ID(a); err != nil || id != 5 {
		t.Fatalf("id %d %v", id, err)
	}
	if u, _ := h.Usage(m.Point, 1); u[5].Inodes != 2 {
		t.Fatalf("usage %+v", u)
	}
	if u, _ := h.Usage(`Q:\`, 1); len(u) != 0 {
		t.Fatal("unknown volume")
	}
	if len(h.volumes()) != 1 {
		t.Fatal("volumes")
	}
	volPathName = func(*uint16, *uint16, uint32) error { return windows.ERROR_PATH_NOT_FOUND }
	_, merr := h.Mount(a)
	_, ierr := h.ID(a)
	aerr := h.Assign(context.Background(), a, 1)
	volPathName = windows.GetVolumePathName
	if merr == nil || ierr == nil || aerr == nil {
		t.Fatal("no volume")
	}
	volInfo = func(root *uint16, _ *uint16, _ uint32, _ *uint32, _ *uint32, _ *uint32, fs *uint16, n uint32) error {
		copy(unsafeSlice(fs, n), windows.StringToUTF16("FAT32"))
		return nil
	}
	m, _ = h.Mount(a)
	_, verr := h.volume(a)
	volInfo = windows.GetVolumeInformation
	if m.Status != "filesystem_not_supported" || verr == nil {
		t.Fatalf("FAT32: %+v %v", m, verr)
	}
	fresh := newHost(quiet())
	ioctl = func(windows.Handle, uint32, *byte, uint32, *byte, uint32, *uint32, *windows.Overlapped) error {
		return errors.New("no journal")
	}
	m, _ = fresh.Mount(a)
	ioctl = windows.DeviceIoControl
	if m.Status != "enable_usn_journal" {
		t.Fatalf("no journal: %+v", m)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	ioctl = func(h windows.Handle, code uint32, in *byte, inLen uint32, out *byte, outLen uint32, n *uint32, o *windows.Overlapped) error {
		return errors.New("broken")
	}
	h.loop(ctx, 5*time.Millisecond, 0)
	ioctl = windows.DeviceIoControl
}

func unsafeSlice(p *uint16, n uint32) []uint16 { return unsafe.Slice(p, n) }

func TestHNS(t *testing.T) {
	saved := hnsCall
	defer func() { hnsCall = saved }()
	hnsCall = func(string, string) (string, error) {
		return `{"Success":true,"Output":[{"ID":"e1","VirtualNetwork":"n1"}]}`, nil
	}
	h := newHost(quiet())
	if eps, err := h.Endpoints(); err != nil || eps[0] != (Endpoint{ID: "e1", Network: "n1"}) {
		t.Fatalf("%v %v", eps, err)
	}
	for _, r := range []string{`{"Success":false,"Error":"x"}`, `{`} {
		hnsCall = func(string, string) (string, error) { return r, nil }
		if _, err := hnsEndpoints(); err == nil {
			t.Fatalf("%s must fail", r)
		}
	}
	hnsCall = func(string, string) (string, error) { return "", errors.New("x") }
	if _, err := hnsEndpoints(); err == nil {
		t.Fatal("call error")
	}
	hnsCall = saved
	saved(http.MethodGet, "/endpoints/")
	saved(http.MethodGet, "/nonexistent-path/")
	proc := procHNSCall
	procHNSCall = windows.NewLazySystemDLL("hatchlens-none.dll").NewProc("HNSCall")
	_, err := saved(http.MethodGet, "/endpoints/")
	procHNSCall = proc
	if err == nil {
		t.Fatal("missing DLL")
	}
}

type tcp struct{ net.Listener }

func tcpListen(t *testing.T) func(string) (net.Listener, error) {
	return func(string) (net.Listener, error) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		return tcp{l}, err
	}
}

type badListener struct{}

// gatedListener fails once release is closed.
type gatedListener struct {
	badListener
	release chan struct{}
}

func (l gatedListener) Accept() (net.Conn, error) { <-l.release; return nil, errors.New("gone") }

func (badListener) Accept() (net.Conn, error) { return nil, errors.New("accept failed") }
func (badListener) Close() error              { return nil }
func (badListener) Addr() net.Addr            { return &net.TCPAddr{} }

func TestServe(t *testing.T) {
	saved := listen
	defer func() { listen = saved }()
	listen = tcpListen(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- serve(ctx, quiet(), "", nil) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	listen = func(string) (net.Listener, error) { return nil, errors.New("pipe in use") }
	if serve(context.Background(), quiet(), "", nil) == nil {
		t.Fatal("listen error")
	}
	listen = func(string) (net.Listener, error) { return badListener{}, nil }
	processToken = func(windows.Handle, uint32, *windows.Token) error { return errors.New("x") }
	err := serve(context.Background(), quiet(), "", nil)
	processToken = windows.OpenProcessToken
	if err == nil {
		t.Fatal("accept error")
	}
	l, err := saved(`\\.\pipe\hatchlens-host-test`)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}

func TestExecute(t *testing.T) {
	saved := listen
	defer func() { listen = saved }()
	listen = tcpListen(t)
	req := make(chan svc.ChangeRequest)
	st := make(chan svc.Status, 10)
	res := make(chan uint32)
	go func() { _, code := service{log: quiet()}.Execute(nil, req, st); res <- code }()
	req <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: svc.Status{State: svc.Running}}
	req <- svc.ChangeRequest{Cmd: svc.Stop}
	if code := <-res; code != 0 {
		t.Fatalf("stop: %d", code)
	}
	listen = func(string) (net.Listener, error) { return nil, errors.New("pipe in use") }
	st = make(chan svc.Status, 10)
	if _, code := (service{log: quiet()}).Execute(nil, make(chan svc.ChangeRequest), st); code != 1 {
		t.Fatalf("failed: %d", code)
	}
	for len(st) > 0 {
		if s := <-st; s.State == svc.Running {
			t.Fatal("running without a listener")
		}
	}
	// A service dying after the start ends with an error.
	release := make(chan struct{})
	listen = func(string) (net.Listener, error) { return gatedListener{release: release}, nil }
	st = make(chan svc.Status, 10)
	go func() { _, code := service{log: quiet()}.Execute(nil, make(chan svc.ChangeRequest), st); res <- code }()
	for s := <-st; s.State != svc.Running; s = <-st {
	}
	close(release)
	if code := <-res; code != 1 {
		t.Fatalf("died: %d", code)
	}
}

func TestMain(t *testing.T) {
	var out, errb bytes.Buffer
	if Main([]string{"version"}, &out, &errb) != 0 || !strings.Contains(out.String(), "hatchlens-host") {
		t.Fatal("version")
	}
	if Main([]string{"x"}, &out, &errb) != 2 {
		t.Fatal("usage")
	}
	defer func() {
		isService, runService, listen, notify, logDir = svc.IsWindowsService, svc.Run, listenPipe, signal.NotifyContext, programData
	}()
	isService = func() (bool, error) { return false, errors.New("x") }
	if Main(nil, &out, &errb) != 1 {
		t.Fatal("isService error")
	}
	dir := t.TempDir()
	logDir = func() string { return dir }
	if DataDir() != dir {
		t.Fatal("DataDir")
	}
	isService = func() (bool, error) { return true, nil }
	runService = func(string, svc.Handler) error { return nil }
	if Main(nil, &out, &errb) != 0 {
		t.Fatal("service")
	}
	runService = func(string, svc.Handler) error { return errors.New("x") }
	if Main(nil, &out, &errb) != 1 {
		t.Fatal("service error")
	}
	os.WriteFile(filepath.Join(dir, "hatchlens-host.log"), make([]byte, 11<<20), 0o644)
	_, f, err := serviceLog()
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if fi, _ := os.Stat(filepath.Join(dir, "hatchlens-host.log")); fi.Size() > 1<<20 {
		t.Fatal("not started fresh")
	}
	file := filepath.Join(dir, "file")
	os.WriteFile(file, nil, 0o644)
	logDir = func() string { return filepath.Join(file, "sub") }
	if Main(nil, &out, &errb) != 1 {
		t.Fatal("log dir")
	}
	logDir = func() string { return dir }
	ro := filepath.Join(dir, "hatchlens-host.log")
	os.Chmod(ro, 0o444)
	_, _, err = serviceLog()
	os.Chmod(ro, 0o644)
	if err == nil {
		t.Fatal("log file")
	}
	isService = func() (bool, error) { return false, nil }
	listen = tcpListen(t)
	notify = func(ctx context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		return c, cancel
	}
	if Main(nil, &out, &errb) != 0 {
		t.Fatal("foreground")
	}
	listen = func(string) (net.Listener, error) { return nil, errors.New("pipe in use") }
	if Main(nil, &out, &errb) != 1 {
		t.Fatal("foreground error")
	}
	if !strings.HasSuffix(programData(), "HatchLens") {
		t.Fatal(programData())
	}
}
