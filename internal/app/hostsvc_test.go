// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/host"
	"github.com/motwok/HatchLens/internal/winhost"
)

type hnsFake struct{ winhost.Backend }

func (hnsFake) Endpoints() ([]winhost.Endpoint, error) {
	return []winhost.Endpoint{{ID: "e1", Network: "aa"}, {ID: "e2", Network: "zz"}}, nil
}

func TestNetNames(t *testing.T) {
	srv := httptest.NewServer(winhost.Handler(hnsFake{}, "t"))
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	svc := host.NewService(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	})
	nets := `[{"Name":"nat","Options":{"com.docker.network.windowsshim.hnsid":"AA"}},{"Name":"none","Options":{}}]`
	dc := docker.NewWithHTTP(handlerRT{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, nets) })})
	got, err := netNames(svc, dc)(context.Background())
	if err != nil || len(got) != 1 || got["E1"] != "nat" {
		t.Fatalf("%v %v", got, err)
	}
	bad := docker.NewWithHTTP(handlerRT{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "x", 500) })})
	if _, err := netNames(svc, bad)(context.Background()); err == nil {
		t.Fatal("networks error")
	}
	down := host.NewService(func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no pipe") })
	if _, err := netNames(down, dc)(context.Background()); err == nil {
		t.Fatal("service error")
	}
}
