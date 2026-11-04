// SPDX-License-Identifier: AGPL-3.0-or-later

package winhost

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// Name is the Windows service name.
const Name = "HatchLensHost"

// Version is set by -ldflags.
var Version = "dev"

// Administrators and SYSTEM, like the Docker engine pipe; a container running
// as ContainerAdministrator reaches it through an npipe mount.
const pipeSDDL = "D:P(A;;GA;;;BA)(A;;GA;;;SY)"

const (
	pollEvery   = 2 * time.Second
	rewalkEvery = time.Hour
)

// Replaceable in tests.
var (
	listen     = listenPipe
	isService  = svc.IsWindowsService
	runService = svc.Run
	notify     = signal.NotifyContext
	logDir     = programData
)

func listenPipe(p string) (net.Listener, error) {
	return winio.ListenPipe(p, &winio.PipeConfig{SecurityDescriptor: pipeSDDL})
}

func programData() string { return filepath.Join(os.Getenv("ProgramData"), "HatchLens") }

type host struct {
	mu   sync.Mutex
	vols map[string]*volume
	log  *slog.Logger
}

func newHost(log *slog.Logger) *host { return &host{vols: map[string]*volume{}, log: log} }

func (h *host) volume(path string) (*volume, error) {
	point, fstype, err := volumeOf(path)
	if err != nil {
		return nil, notExist(err)
	}
	if fstype != "NTFS" {
		return nil, errf("%s is %s, not NTFS", point, fstype)
	}
	key := volKey(point)
	h.mu.Lock()
	defer h.mu.Unlock()
	if v := h.vols[key]; v != nil {
		return v, nil
	}
	v, err := openVolume(point)
	if err != nil {
		return nil, err
	}
	h.vols[key] = v
	return v, nil
}

// volKey: one volume per mount point, `C:\` and `C:` alike (HatchLens
// sends the point without the trailing backslash).
func volKey(point string) string { return strings.ToUpper(strings.TrimSuffix(point, `\`)) }

func (h *host) Endpoints() ([]Endpoint, error) { return hnsEndpoints() }

// Stat: only a missing path is gone; any other error counts as present, so
// HatchLens releases nothing on it.
func (h *host) Stat(path string) Stat {
	fi, err := os.Stat(path)
	if err != nil {
		gone := errors.Is(err, fs.ErrNotExist) || errors.Is(err, windows.ERROR_DIRECTORY)
		return Stat{Exists: !gone, Dir: !gone}
	}
	return Stat{Exists: true, Dir: fi.IsDir()}
}

func (h *host) Dirs(path string) ([]string, error) {
	es, err := list(path)
	if err != nil {
		return nil, notExist(err)
	}
	out := []string{}
	for _, e := range es {
		if e.attrs&attrDirectory != 0 && e.attrs&attrReparse == 0 {
			out = append(out, e.name)
		}
	}
	return out, nil
}

func (h *host) Mount(path string) (Mount, error) {
	point, fstype, err := volumeOf(path)
	if err != nil {
		return Mount{}, notExist(err)
	}
	m := Mount{Point: point, FSType: fstype}
	switch {
	case fstype != "NTFS":
		m.Status = "filesystem_not_supported"
	default:
		if _, err := h.volume(path); err != nil {
			h.log.Warn("volume not usable", "volume", point, "err", err)
			m.Status = "enable_usn_journal"
		}
	}
	return m, nil
}

func (h *host) ID(path string) (uint32, error) {
	v, err := h.volume(path)
	if err != nil {
		return 0, err
	}
	return v.id(path)
}

func (h *host) Assign(ctx context.Context, path string, id uint32) error {
	v, err := h.volume(path)
	if err != nil {
		return err
	}
	h.log.Info("assign", "path", path, "id", id)
	return v.assign(ctx, path, id)
}

func (h *host) Usage(volume string, min uint32) (map[uint32]Usage, error) {
	h.mu.Lock()
	v := h.vols[volKey(volume)]
	h.mu.Unlock()
	if v == nil {
		return map[uint32]Usage{}, nil
	}
	return v.usage(min), nil
}

func (h *host) volumes() []*volume {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*volume, 0, len(h.vols))
	for _, v := range h.vols {
		out = append(out, v)
	}
	return out
}

func (h *host) loop(ctx context.Context, poll, rewalk time.Duration) {
	walked := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
		again := time.Since(walked) >= rewalk
		for _, v := range h.volumes() {
			if again {
				v.rewalk()
			}
			if err := v.poll(); err != nil {
				h.log.Warn("journal read failed", "volume", v.point, "err", err)
			}
		}
		if again {
			walked = time.Now()
		}
	}
}

// Start runs the host part in this process until ctx ends: in the service,
// and in HatchLens itself when it runs as a Windows service on the host.
func Start(ctx context.Context, log *slog.Logger) Backend {
	if err := enableBackup(); err != nil {
		log.Warn("SeBackupPrivilege not enabled", "err", err)
	}
	h := newHost(log)
	go h.loop(ctx, pollEvery, rewalkEvery)
	return h
}

// serve listens on the pipe and, with tcp set, on that loopback address with
// the token; ready, if set, is called once the listeners are up.
func serve(ctx context.Context, log *slog.Logger, tcp string, ready func()) error {
	var tl net.Listener
	tok := ""
	if tcp != "" {
		var err error
		if err = loopback(tcp); err == nil {
			if tok, err = Token(); err == nil {
				tl, err = net.Listen("tcp", tcp)
			}
		}
		if err != nil {
			return err
		}
	}
	l, err := listen(Pipe)
	if err != nil {
		if tl != nil {
			tl.Close()
		}
		return err
	}
	h := Handler(Start(ctx, log), Version)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	tsrv := &http.Server{Handler: auth(tok, h), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
		tsrv.Close()
	}()
	if tl != nil {
		go tsrv.Serve(tl)
		log.Info("listening", "tcp", tcp)
	}
	log.Info("listening", "pipe", Pipe, "version", Version)
	if ready != nil {
		ready()
	}
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type service struct {
	log *slog.Logger
	tcp string
}

func (s service) Execute(_ []string, req <-chan svc.ChangeRequest, st chan<- svc.Status) (bool, uint32) {
	st <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, ready := make(chan error, 1), make(chan struct{}, 1)
	go func() { done <- serve(ctx, s.log, s.tcp, func() { ready <- struct{}{} }) }()
	// Running only with the listeners up, so a failed start is one for the
	// service control manager and the installer too.
	select {
	case <-ready:
	case err := <-done:
		s.log.Error("not started", "err", err)
		return false, 1
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
		case err := <-done:
			s.log.Error("stopped", "err", err)
			return false, 1
		}
	}
}

// DataDir is %ProgramData%\HatchLens, for logs, and the config and state of
// HatchLens as a Windows service.
func DataDir() string { return logDir() }

// OpenLog opens a log file in DataDir, started fresh once it passed 10 MB.
func OpenLog(name string) (*os.File, error) {
	dir := logDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	name = filepath.Join(dir, name)
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(name); err == nil && fi.Size() > 10<<20 {
		flag |= os.O_TRUNC
	}
	return os.OpenFile(name, flag, 0o644)
}

func serviceLog() (*slog.Logger, io.Closer, error) {
	f, err := OpenLog("hatchlens-host.log")
	if err != nil {
		return nil, nil, err
	}
	return slog.New(slog.NewTextHandler(f, nil)), f, nil
}

// Main runs the service under the service control manager, or in the
// foreground until Ctrl+C.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, "hatchlens-host", Version)
		return 0
	}
	if len(args) > 0 && args[0] == "enable-quota" {
		fs := flag.NewFlagSet("enable-quota", flag.ContinueOnError)
		fs.SetOutput(stderr)
		vhd := fs.String("vhd", "", `data disk of Docker Desktop (default: found under %LOCALAPPDATA%\Docker\wsl)`)
		if fs.Parse(args[1:]) != nil || fs.NArg() > 0 {
			return 2
		}
		if err := EnableQuota(*vhd, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	fs := flag.NewFlagSet("hatchlens-host", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tcp := fs.String("tcp", "", "")
	if fs.Parse(args) != nil || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: hatchlens-host [-tcp 127.0.0.1:PORT] | version | enable-quota [-vhd PATH]")
		return 2
	}
	inService, err := isService()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if inService {
		log, f, err := serviceLog()
		if err != nil {
			return 1
		}
		defer f.Close()
		if err := runService(Name, service{log, *tcp}); err != nil {
			log.Error("service failed", "err", err)
			return 1
		}
		return 0
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := notify(context.Background(), os.Interrupt)
	defer stop()
	if err := serve(ctx, log, *tcp, nil); err != nil {
		log.Error("serve failed", "err", err)
		return 1
	}
	return 0
}
