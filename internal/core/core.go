// SPDX-License-Identifier: AGPL-3.0-or-later

// Package core discovers entries, assigns project IDs and produces the metrics.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/motwok/DirAdvisor/internal/config"
	"github.com/motwok/DirAdvisor/internal/docker"
	"github.com/motwok/DirAdvisor/internal/host"
	"github.com/motwok/DirAdvisor/internal/state"
)

// Docker is the part of the Docker API DirAdvisor uses.
type Docker interface {
	CheckVersion(ctx context.Context) (string, bool, error)
	Info(ctx context.Context) (docker.Info, error)
	Volumes(ctx context.Context) ([]docker.Volume, error)
	Containers(ctx context.Context) ([]docker.Container, error)
	Container(ctx context.Context, id string) (docker.ContainerJSON, error)
	ImageRepoDigests(ctx context.Context, id string) ([]string, error)
	DistributionDigest(ctx context.Context, ref string) (string, error)
}

// ErrDockerTooOld stops DirAdvisor (spec DIS-14).
var ErrDockerTooOld = errors.New("docker API too old")

type Options struct {
	FS         host.FS
	Docker     Docker
	StorageDir string
	SelfID     string
	Log        *slog.Logger
	Now        func() time.Time
}

type Core struct {
	mu       sync.Mutex
	cfg      *config.Config
	st       *state.State
	saved    []byte
	fs       host.FS
	dk       Docker
	storage  string
	self     string
	log      *slog.Logger
	now      func() time.Time
	view     view
	walkable map[string]bool
	// tops are the paths of all candidates of the last reconcile; walks do
	// not enter them.
	tops   map[string]bool
	failed map[string]time.Time
	wake   chan struct{}

	versionOK  bool
	dockerRoot string
	// rootKnown is false while Docker is enabled but its root directory was
	// never seen; configured directories wait, so no walk can enter it.
	rootKnown  bool
	ready      bool
	recErrors  float64
	recSeconds float64
	reloadOK   bool
	logged     map[string]time.Time
	img        imageState
}

type entryView struct {
	Key, Path, Source, Volume, Stack, Owner, State string
	ProjectID                                      uint32
	Labels                                         map[string]string
	Mount                                          host.Mount
	Assigning                                      bool
	LastUsed                                       int64
}

type unmeasurable struct{ Path, Reason string }

type adminAction struct{ Mountpoint, FSType, Action, Command string }

type overlap struct{ Outer, Inner string }

type view struct {
	entries []entryView
	unmeas  []unmeasurable
	actions []adminAction
	overlap []overlap
}

// New creates the core with a loaded state. A state load error is logged and an
// empty state is used (spec STA-4).
func New(cfg *config.Config, o Options) *Core {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	st, err := state.Load(o.StorageDir)
	if err != nil {
		o.Log.Error("state reset", "err", err)
	}
	saved, _ := json.Marshal(st)
	return &Core{
		cfg: cfg, st: st, saved: saved, fs: o.FS, dk: o.Docker, storage: o.StorageDir, self: o.SelfID,
		log: o.Log, now: o.Now, walkable: map[string]bool{}, failed: map[string]time.Time{},
		wake: make(chan struct{}, 1), reloadOK: true, logged: map[string]time.Time{},
		dockerRoot: st.DockerRoot, rootKnown: st.DockerRoot != "" || !cfg.Docker.Enabled || o.Docker == nil,
	}
}

// SetConfig activates a new config after a successful reload.
func (c *Core) SetConfig(cfg *config.Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg
	c.reloadOK = true
}

// ReloadFailed records a failed reload; the previous config stays.
func (c *Core) ReloadFailed() {
	c.mu.Lock()
	c.reloadOK = false
	c.mu.Unlock()
}

func (c *Core) Config() *config.Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

func (c *Core) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

// DockerRoot returns the DockerRootDir seen last (empty before).
func (c *Core) DockerRoot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dockerRoot
}

func (c *Core) saveLocked() {
	data, err := json.Marshal(c.st)
	if err != nil || string(data) == string(c.saved) {
		return
	}
	if err := c.st.Save(c.storage); err != nil {
		c.log.Error("writing state failed", "err", err)
		return
	}
	c.saved = data
}

// Save writes the state if it changed.
func (c *Core) Save() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saveLocked()
}

func (c *Core) wakeWalker() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}
