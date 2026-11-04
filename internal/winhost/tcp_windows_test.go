// SPDX-License-Identifier: AGPL-3.0-or-later

package winhost

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestLoopback(t *testing.T) {
	if loopback("127.0.0.1:8091") != nil || loopback("[::1]:1") != nil {
		t.Fatal("loopback refused")
	}
	for _, a := range []string{"bad", "10.0.0.1:1", "localhost:1"} {
		if loopback(a) == nil {
			t.Errorf("%s accepted", a)
		}
	}
}

func TestToken(t *testing.T) {
	saved := logDir
	defer func() { logDir = saved }()
	trustOwner(t)
	dir := t.TempDir()
	logDir = func() string { return dir }
	a, err := Token()
	if err != nil || len(a) != 64 {
		t.Fatalf("%q %v", a, err)
	}
	if b, _ := Token(); b != a {
		t.Fatal("token changed")
	}
	os.Remove(filepath.Join(dir, TokenFile))
	os.Mkdir(filepath.Join(dir, TokenFile), 0o755)
	if _, err := Token(); err == nil {
		t.Fatal("token path is a directory")
	}
	file := filepath.Join(dir, "file")
	os.WriteFile(file, nil, 0o644)
	logDir = func() string { return filepath.Join(file, "sub") }
	if _, err := Token(); err == nil {
		t.Fatal("data dir")
	}
	logDir = func() string { return dir }
	fileOwner = func(string) (*windows.SID, error) { return windows.CreateWellKnownSid(windows.WinWorldSid) }
	if _, err := Token(); err == nil {
		t.Fatal("planted token")
	}
}

func TestAuth(t *testing.T) {
	h := auth("tok", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	for hdr, want := range map[string]int{"": 401, "Bearer x": 401, "Bearer tok": 200} {
		r := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
		if hdr != "" {
			r.Header.Set("Authorization", hdr)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%q: %d", hdr, w.Code)
		}
	}
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestServeTCP(t *testing.T) {
	sl, sd := listen, logDir
	defer func() { listen, logDir = sl, sd }()
	trustOwner(t)
	dir := t.TempDir()
	logDir = func() string { return dir }
	listen = tcpListen(t)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- serve(ctx, quiet(), addr) }()
	tok := ""
	var code int
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		tok, _ = Token()
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/version", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			code = resp.StatusCode
			resp.Body.Close()
			break
		}
	}
	cancel()
	if err := <-done; err != nil || code != 200 {
		t.Fatalf("%v %d", err, code)
	}

	if serve(context.Background(), quiet(), "10.0.0.1:1") == nil {
		t.Fatal("not loopback")
	}
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	if serve(context.Background(), quiet(), busy.Addr().String()) == nil {
		t.Fatal("port in use")
	}
	listen = func(string) (net.Listener, error) { return nil, errors.New("pipe in use") }
	if serve(context.Background(), quiet(), freePort(t)) == nil {
		t.Fatal("pipe error")
	}
}

func TestMainTCPUsage(t *testing.T) {
	var out, errb discard
	if Main([]string{"-tcp"}, &out, &errb) != 2 {
		t.Fatal("missing value")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
