// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package docker

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ApiVersion":"1.47"}`)
	}))
	srv.Listener = l
	srv.Start()
	defer srv.Close()
	if v, _, err := New(sock).CheckVersion(context.Background()); err != nil || v != "1.47" {
		t.Fatalf("%q %v", v, err)
	}
	if DefaultSocket != "/var/run/docker.sock" {
		t.Fatal(DefaultSocket)
	}
}
