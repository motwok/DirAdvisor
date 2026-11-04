// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The Docker engine pipe exists on the Windows CI runners and with Docker Desktop.
func TestPipe(t *testing.T) {
	if _, err := os.Stat(DefaultSocket); err != nil {
		t.Skip("no Docker pipe")
	}
	c := New(DefaultSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, reached, err := c.CheckVersion(ctx); !reached {
		t.Fatalf("pipe not reached: %v", err)
	}
	// Two requests at once: overlapped I/O must not deadlock.
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := c.Containers(ctx); done <- err }()
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	conn, err := dialer(DefaultSocket)(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.LocalAddr().Network() != "pipe" || conn.RemoteAddr().String() != DefaultSocket {
		t.Fatalf("addr %v %v", conn.LocalAddr(), conn.RemoteAddr())
	}
}

func TestPipeErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := dialer(`\\.\pipe\imageadvisor-test-missing`)(ctx, "", ""); err == nil {
		t.Fatal("missing pipe")
	}
	if _, err := dialer("bad\x00name")(ctx, "", ""); err == nil {
		t.Fatal("bad name")
	}
	orig := openPipe
	defer func() { openPipe = orig }()
	calls := 0
	openPipe = func(name string) (*os.File, error) {
		if name != "x" {
			return orig(name)
		}
		calls++
		if calls == 1 {
			return nil, errorPipeBusy
		}
		return os.Open(os.Args[0])
	}
	conn, err := dialer("x")(ctx, "", "")
	if err != nil || calls != 2 {
		t.Fatalf("busy retry: %v %d", err, calls)
	}
	if conn.LocalAddr().Network() != "pipe" || conn.RemoteAddr().String() != "x" {
		t.Fatalf("addr %v %v", conn.LocalAddr(), conn.RemoteAddr())
	}
	conn.Close()
	openPipe = func(string) (*os.File, error) { return nil, errorPipeBusy }
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := dialer("x")(cctx, "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

// A server must not get the client's identity: the dialer asks for anonymous
// SQOS, so the impersonation token is anonymous and cannot even be opened.
func TestPipeAnonymous(t *testing.T) {
	name := fmt.Sprintf(`\\.\pipe\hatchlens-test-%d`, os.Getpid())
	p, _ := windows.UTF16PtrFromString(name)
	h, err := windows.CreateNamedPipe(p, windows.PIPE_ACCESS_DUPLEX, windows.PIPE_TYPE_BYTE|windows.PIPE_WAIT, 1, 4096, 4096, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	impersonate := windows.NewLazySystemDLL("advapi32.dll").NewProc("ImpersonateNamedPipeClient")
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := windows.ConnectNamedPipe(h, nil); err != nil && err != windows.ERROR_PIPE_CONNECTED {
			result <- err
			return
		}
		// Impersonation is only possible after a read from the pipe.
		var n uint32
		if err := windows.ReadFile(h, make([]byte, 1), &n, nil); err != nil {
			result <- err
			return
		}
		if r, _, err := impersonate.Call(uintptr(h)); r == 0 {
			result <- err
			return
		}
		defer windows.RevertToSelf()
		var tok windows.Token
		err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &tok)
		if err == nil {
			tok.Close()
		}
		result <- err
	}()
	conn, err := dialer(name)(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, windows.ERROR_CANT_OPEN_ANONYMOUS) {
		t.Fatalf("impersonation: %v", err)
	}
}
