// SPDX-License-Identifier: AGPL-3.0-or-later

// Package app wires config, core and the HTTP server.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/core"
	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/host"
	"github.com/motwok/HatchLens/internal/metrics"
)

// Set by -ldflags.
var (
	Version   = "dev"
	Revision  = "unknown"
	BuildTime = "0"
)

// ParseFlags parses the command line (spec section 3) and returns the -config path. Errors mean exit 2.
func ParseFlags(args []string, out io.Writer) (string, error) {
	fs := flag.NewFlagSet("hatchlens", flag.ContinueOnError)
	fs.SetOutput(out)
	path := fs.String("config", "", "config file (else HATCHLENS_CONFIG_FILE or HATCHLENS_CONFIG)")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return *path, nil
}

// NewLogger takes level and format as validated by the config.
func NewLogger(level, format string, w io.Writer) *slog.Logger {
	var lvl slog.Level
	lvl.UnmarshalText([]byte(level))
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func buildInfo() core.BuildInfo {
	var t int64
	fmt.Sscan(BuildTime, &t)
	return core.BuildInfo{Version: Version, Revision: Revision, GoVersion: runtime.Version(), BuildTime: t}
}

// Server holds the running parts.
type Server struct {
	Log     *slog.Logger
	Core    *core.Core
	FS      host.FS
	trigger chan struct{}
}

// Handler serves /metrics and the lifecycle endpoints (spec section 12).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", metrics.ContentType)
		metrics.Write(w, s.Core.Collect(r.Context(), buildInfo()))
	})
	mux.HandleFunc("/-/healthy", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "HatchLens is healthy.\n")
	})
	mux.HandleFunc("/-/ready", func(w http.ResponseWriter, _ *http.Request) {
		if !s.Core.Ready() {
			http.Error(w, "HatchLens is not ready.", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "HatchLens is ready.\n")
	})
	return mux
}

// Main runs HatchLens and returns the exit code.
func Main(args []string, stderr io.Writer) int {
	path, err := ParseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 2
	}
	data, src, err := config.Source(path, os.Getenv)
	var cfg *config.Config
	var fsys host.FS
	if err == nil {
		cfg, err = config.Parse(data, os.LookupEnv, nil)
	}
	if err == nil {
		fsys = host.New(cfg.Global.RootFS)
		err = cfg.CheckDirectories(fsys.Stat)
	}
	if err != nil {
		NewLogger("info", "logfmt", stderr).Error("invalid config", "source", src, "err", err)
		return 1
	}
	g := cfg.Global
	log := NewLogger(g.LogLevel, g.LogFormat, stderr)
	s := &Server{Log: log, FS: fsys, trigger: make(chan struct{}, 1)}
	var dk core.Docker
	var dc *docker.Client
	if cfg.Docker.Socket != "" {
		dc = docker.New(cfg.Docker.Socket)
		dk = dc
	}
	selfID := ""
	if g.RootFS != "/" {
		selfID = host.SelfContainerID()
	}
	s.Core = core.New(cfg, core.Options{FS: fsys, Docker: dk, StorageDir: g.StoragePath, SelfID: selfID, Log: log})
	log.Info("starting", "version", Version, "revision", Revision, "listen", g.ListenAddress, "config", src)
	if mod, user := fsys.ZFSVersions(); mod != "" || user != "" {
		log.Info("ZFS versions", "module", mod, "userland", user)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	signal.Ignore(syscall.SIGHUP)

	ln, err := net.Listen("tcp", g.ListenAddress)
	if err != nil {
		log.Error("listen failed", "err", err)
		return 1
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, WriteTimeout: time.Minute, IdleTimeout: 2 * time.Minute}
	go srv.Serve(ln)

	walkDone := make(chan struct{})
	go func() { s.Core.RunWalker(ctx); close(walkDone) }()
	go s.Core.RunImageCheck(ctx)
	if dc != nil {
		go s.watchEvents(ctx, dc, time.Second, time.Minute)
	}
	code := s.loop(ctx)

	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	waitDone(shutdown, walkDone)
	s.Core.SaveWithin(shutdown)
	srv.Shutdown(shutdown)
	log.Info("stopped")
	return code
}

func waitDone(ctx context.Context, done <-chan struct{}) {
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (s *Server) loop(ctx context.Context) int {
	for ctx.Err() == nil {
		// Reconcile only fails for a too old Docker; other errors are counted.
		if err := s.Core.Reconcile(ctx); err != nil {
			s.Log.Error("unsupported Docker", "err", err)
			return 1
		}
		timer := time.NewTimer(s.Core.Config().Global.ReconcileInterval.D())
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0
		case <-timer.C:
		case <-s.trigger:
			timer.Stop()
		}
	}
	return 0
}

func (s *Server) poke() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// watchEvents triggers a reconcile on relevant Docker events (spec DIS-10/11).
func (s *Server) watchEvents(ctx context.Context, dc *docker.Client, minRetry, maxRetry time.Duration) {
	backoff := minRetry
	for ctx.Err() == nil {
		start := time.Now()
		err := dc.Events(ctx, func(e docker.Event) {
			if e.Relevant() {
				s.poke()
			}
		})
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > maxRetry {
			backoff = minRetry
		}
		s.Log.Debug("Docker event stream lost", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxRetry {
			backoff = maxRetry
		}
	}
}
