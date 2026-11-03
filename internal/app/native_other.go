// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package app

import (
	"context"
	"log/slog"
	"os"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/host"
)

// HatchLens runs as a service only on Windows.
func asService([]string) (int, bool) { return 0, false }

func configEnv(bool) func(string) string { return os.Getenv }

func nativeStorage(*config.Config, bool) {}

func hostService(context.Context, *config.Config, *slog.Logger, bool) *host.Service { return nil }
