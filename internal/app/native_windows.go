// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/svc"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/host"
	"github.com/motwok/HatchLens/internal/winhost"
)

// ServiceName is the Windows service of HatchLens on the host.
const ServiceName = "HatchLens"

// Replaceable in tests.
var (
	isService  = svc.IsWindowsService
	runService = svc.Run
	startHost  = winhost.Start
	dataDir    = winhost.DataDir
	openLog    = winhost.OpenLog
	checkOwner = winhost.CheckOwner
)

// asService runs HatchLens under the service control manager when it was
// started as the Windows service; the log goes to hatchlens.log in DataDir.
func asService(args []string) (int, bool) {
	in, err := isService()
	if err != nil || !in {
		return 0, false
	}
	f, err := openLog("hatchlens.log")
	if err != nil {
		return 1, true
	}
	defer f.Close()
	if err := runService(ServiceName, handler{args: args, w: f}); err != nil {
		return 1, true
	}
	return 0, true
}

type handler struct {
	args []string
	w    io.Writer
}

func (h handler) Execute(_ []string, req <-chan svc.ChangeRequest, st chan<- svc.Status) (bool, uint32) {
	st <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, ready := make(chan int, 1), make(chan struct{}, 1)
	go func() { done <- serve(ctx, cancel, h.args, h.w, true, func() { ready <- struct{}{} }) }()
	// Running only with the listener up, so a failed start is one for the
	// service control manager and the installer too.
	select {
	case <-ready:
	case code := <-done:
		return false, uint32(code)
	}
	st <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				st <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				st <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		case code := <-done:
			return false, uint32(code)
		}
	}
}

// configEnv: as a service without -config and without the variables, the
// config is DataDir\config.yml when it exists.
func configEnv(native bool) func(string) string {
	if !native {
		return os.Getenv
	}
	return func(k string) string {
		v := os.Getenv(k)
		if v == "" && k == "HATCHLENS_CONFIG_FILE" && os.Getenv("HATCHLENS_CONFIG") == "" {
			f := filepath.Join(dataDir(), "config.yml")
			if _, err := os.Stat(f); err == nil {
				return f
			}
		}
		return v
	}
}

// dataOwner: as a service, DataDir and a config.yml in it must come from
// SYSTEM or an administrator, else any user could configure the service.
func dataOwner(native bool) error {
	if !native {
		return nil
	}
	for _, p := range []string{dataDir(), filepath.Join(dataDir(), "config.yml")} {
		if err := checkOwner(p); err != nil {
			return err
		}
	}
	return nil
}

// nativeStorage: as a service the default state directory is DataDir.
func nativeStorage(cfg *config.Config, native bool) {
	if native && cfg.Global.StoragePath == config.Default().Global.StoragePath {
		cfg.Global.StoragePath = dataDir()
	}
}

// hostService is the host part: in this process as a service on the host,
// else over the pipe of the HatchLens Host service.
func hostService(ctx context.Context, cfg *config.Config, log *slog.Logger, native bool) *host.Service {
	if native {
		return host.NewLocal(startHost(ctx, log))
	}
	if cfg.HostSocket != "" {
		return host.NewService(cfg.HostToken, docker.Dial(cfg.HostSocket))
	}
	return nil
}

// withHost: on Windows the service is the whole FS.
func withHost(_ host.FS, svc *host.Service) host.FS { return svc }
