// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package app

import (
	"context"
	"log/slog"
	"os"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/host"
)

// HatchLens runs as a service only on Windows.
func asService([]string) (int, bool) { return 0, false }

func configEnv(bool) func(string) string { return os.Getenv }

func dataOwner(bool) error { return nil }

func nativeStorage(*config.Config, bool) {}

// hostService: under Docker Desktop the HatchLens Host service on the Windows
// host measures directories on Windows drives, over TCP.
func hostService(_ context.Context, cfg *config.Config, _ *slog.Logger, _ bool) *host.Service {
	if cfg.HostSocket == "" {
		return nil
	}
	return host.NewService(cfg.HostToken, docker.Dial(cfg.HostSocket))
}

// withHost keeps the local project quotas for everything but Windows drives.
func withHost(local host.FS, svc *host.Service) host.FS { return host.Split{Local: local, Remote: svc} }
