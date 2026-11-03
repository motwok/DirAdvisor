// SPDX-License-Identifier: AGPL-3.0-or-later

// Package app wires config, both parts and the HTTP server.
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
	"github.com/motwok/HatchLens/internal/images"
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

// Server holds the running parts. Core (directories) is nil on Windows, Images
// is nil when the image checks are off.
type Server struct {
	Log      *slog.Logger
	Core     *core.Core
	Images   *images.Core
	FS       host.FS
	trigger  chan struct{}
	itrigger chan struct{}
}

func newServer(log *slog.Logger) *Server {
	return &Server{Log: log, trigger: make(chan struct{}, 1), itrigger: make(chan struct{}, 1)}
}

func one(name, help, typ string, v float64) metrics.Family {
	return metrics.Family{Name: "hatchlens_" + name, Help: help, Type: typ, Samples: []metrics.Sample{{Value: v}}}
}

// collect returns the families of both parts and the shared ones (spec section 11).
func (s *Server) collect(ctx context.Context) []metrics.Family {
	var fams []metrics.Family
	var seconds, errs float64
	if s.Core != nil {
		fams = append(fams, s.Core.Collect(ctx)...)
		d, e := s.Core.Stats()
		seconds, errs = seconds+d, errs+e
	}
	if s.Images != nil {
		fams = append(fams, s.Images.Collect()...)
		d, e := s.Images.Stats()
		seconds, errs = seconds+d, errs+e
	}
	var t int64
	fmt.Sscan(BuildTime, &t)
	return append(fams,
		one("reconcile_duration_seconds", "Duration of the last reconciles.", "gauge", seconds),
		one("reconcile_errors_total", "Failed reconciles.", "counter", errs),
		metrics.Family{Name: "hatchlens_build_info", Help: "Build information.", Type: "gauge", Samples: []metrics.Sample{{
			Labels: map[string]string{"version": Version, "revision": Revision, "goversion": runtime.Version()}, Value: 1}}},
		one("build_timestamp_seconds", "Build time of the binary.", "gauge", float64(t)),
	)
}

func (s *Server) ready() bool {
	return (s.Core == nil || s.Core.Ready()) && (s.Images == nil || s.Images.Ready())
}

// Handler serves /metrics and the lifecycle endpoints (spec section 12).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", metrics.ContentType)
		metrics.Write(w, s.collect(r.Context()))
	})
	mux.HandleFunc("/-/healthy", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "HatchLens is healthy.\n")
	})
	mux.HandleFunc("/-/ready", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready() {
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
	if err == nil {
		cfg, err = config.Parse(data, os.LookupEnv, nil)
	}
	var fsys host.FS
	if err == nil && (cfg.Enabled(config.FnDirectory) || cfg.Enabled(config.FnVolume)) {
		fsys = host.New(cfg.Global.RootFS)
		if cfg.Enabled(config.FnDirectory) {
			err = cfg.CheckDirectories(fsys.Stat)
		}
	}
	if err != nil {
		NewLogger("info", "logfmt", stderr).Error("invalid config", "source", src, "err", err)
		return 1
	}
	g := cfg.Global
	log := NewLogger(g.LogLevel, g.LogFormat, stderr)
	s := newServer(log)
	s.FS = fsys
	var dc *docker.Client
	if cfg.DockerSocket != "" {
		dc = docker.New(cfg.DockerSocket)
	}
	if fsys != nil {
		var dk core.Docker
		if dc != nil {
			dk = dc
		}
		selfID := ""
		if g.RootFS != "/" {
			selfID = host.SelfContainerID()
		}
		s.Core = core.New(cfg, core.Options{FS: fsys, Docker: dk, StorageDir: g.StoragePath, SelfID: selfID, Log: log})
		if mod, user := fsys.ZFSVersions(); mod != "" || user != "" {
			log.Info("ZFS versions", "module", mod, "userland", user)
		}
	}
	// An empty socket with the image part on is a config error.
	if cfg.Enabled(config.FnImage) {
		s.Images = images.New(cfg, images.Options{Docker: dc, StorageDir: g.StoragePath, Log: log})
	}
	log.Info("starting", "version", Version, "revision", Revision, "listen", g.ListenAddress, "config", src,
		"directories", s.Core != nil, "images", s.Images != nil)

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
	if dc != nil {
		go s.watchEvents(ctx, dc, time.Second, time.Minute)
	}
	code := s.run(ctx, stop)
	closing, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	srv.Shutdown(closing)
	log.Info("stopped")
	return code
}

// run starts the loops of both parts and blocks until ctx ends or a part stops
// with a too old Docker; then it writes the states within 8 seconds.
func (s *Server) run(ctx context.Context, stop context.CancelFunc) int {
	codes := make(chan int, 2)
	done := make(chan struct{}, 2)
	loops := 0
	if s.Core != nil {
		loops++
		go func() { s.Core.RunWalker(ctx); done <- struct{}{} }()
		go func() { codes <- s.loop(ctx) }()
	}
	if s.Images != nil {
		loops++
		go func() { s.Images.RunChecks(ctx); done <- struct{}{} }()
		go func() { codes <- s.imageLoop(ctx) }()
	}
	code := 0
	if loops == 0 {
		<-ctx.Done()
	}
	for i := 0; i < loops; i++ {
		if c := <-codes; c != 0 {
			code = c
			stop()
		}
	}
	stop()
	budget, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for i := 0; i < loops; i++ {
		waitDone(budget, done)
	}
	if s.Core != nil {
		s.Core.SaveWithin(budget)
	}
	if s.Images != nil {
		s.Images.Save()
	}
	return code
}

func waitDone(ctx context.Context, done <-chan struct{}) {
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// loop runs the directory reconciles until ctx ends; 1 means the Docker API is too old.
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

// imageLoop runs the image reconciles until ctx ends; 1 means the Docker API is too old.
func (s *Server) imageLoop(ctx context.Context) int {
	for ctx.Err() == nil {
		if err := s.Images.Reconcile(ctx); err != nil {
			s.Log.Error("unsupported Docker", "err", err)
			return 1
		}
		timer := time.NewTimer(s.Images.Config().Global.ReconcileInterval.D())
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0
		case <-timer.C:
		case <-s.itrigger:
			timer.Stop()
		case <-s.Images.ReconcileRequests():
			timer.Stop()
		}
	}
	return 0
}

func (s *Server) poke() {
	for _, ch := range []chan struct{}{s.trigger, s.itrigger} {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// watchEvents triggers the reconciles on relevant Docker events (spec DIS-10/11).
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
