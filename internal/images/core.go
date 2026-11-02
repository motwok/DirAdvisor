// SPDX-License-Identifier: AGPL-3.0-or-later

// Package images finds the containers, checks their image references and produces the image metrics.
package images

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/images/state"
)

// Docker is the part of the Docker API HatchLens uses.
type Docker interface {
	CheckVersion(ctx context.Context) (string, bool, error)
	Containers(ctx context.Context) ([]docker.Container, error)
	Container(ctx context.Context, id string) (docker.ContainerJSON, error)
	Image(ctx context.Context, idOrRef string) (docker.Image, error)
	Distribution(ctx context.Context, ref, auth string) (string, error)
}

// ErrDockerTooOld stops HatchLens (DIS-9).
var ErrDockerTooOld = errors.New("docker API too old")

type Options struct {
	Docker     Docker
	StorageDir string
	Log        *slog.Logger
	Now        func() time.Time
	// Rand returns a random value in [0, n).
	Rand func(n int64) int64
}

// entry is one reported container.
type entry struct {
	Container, Image, Ref, Stack, State, Mode string
	Labels                                    map[string]string
	// -1: not reported.
	Available, Pulled int
	Since             int64
	running           []string
}

type Core struct {
	mu      sync.Mutex
	cfg     *config.Config
	st      *state.State
	saved   []byte
	storage string
	dk      Docker
	log     *slog.Logger
	now     func() time.Time
	rand    func(int64) int64

	versionOK   bool
	ready       bool
	recErrors   float64
	recSeconds  float64
	view        []entry
	cfgImages   map[string]string
	prev        map[string]entry
	warned      map[string]bool
	lastReq     map[string]time.Time
	saveErrLog  time.Time
	reconcileCh chan struct{}
	checkCh     chan struct{}
}

// New creates the core with a loaded state. A state load error is logged and an
// empty state is used (STA-5).
func New(cfg *config.Config, o Options) *Core {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.Int64N
	}
	st, err := state.Load(o.StorageDir)
	if err != nil {
		o.Log.Error("state reset", "err", err)
	}
	saved, _ := json.Marshal(st)
	return &Core{
		cfg: cfg, st: st, saved: saved, storage: o.StorageDir, dk: o.Docker, log: o.Log, now: o.Now,
		rand: o.Rand, cfgImages: map[string]string{}, prev: map[string]entry{},
		warned: map[string]bool{}, lastReq: map[string]time.Time{},
		reconcileCh: make(chan struct{}, 1), checkCh: make(chan struct{}, 1),
	}
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

// ReconcileRequests delivers a value when a registry check asks for a reconcile.
func (c *Core) ReconcileRequests() <-chan struct{} { return c.reconcileCh }

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (c *Core) saveLocked() {
	data, err := json.Marshal(c.st)
	if err != nil || string(data) == string(c.saved) {
		return
	}
	if err := c.st.Save(c.storage); err != nil {
		if now := c.now(); now.Sub(c.saveErrLog) >= time.Hour {
			c.saveErrLog = now
			c.log.Error("writing state failed", "err", err)
		}
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
