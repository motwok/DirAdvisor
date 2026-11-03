// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package docker

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
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
