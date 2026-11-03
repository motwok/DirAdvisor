// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package app

import (
	"context"
	"testing"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/host"
)

func TestHostServiceDockerDesktop(t *testing.T) {
	if hostService(context.Background(), &config.Config{}, nil, false) != nil {
		t.Fatal("no host_socket, no service")
	}
	svc := hostService(context.Background(), &config.Config{HostSocket: "tcp://127.0.0.1:1", HostToken: "t"}, nil, false)
	if svc == nil {
		t.Fatal("service")
	}
	if _, ok := withHost(nil, svc).(host.Split); !ok {
		t.Fatal("split")
	}
}
