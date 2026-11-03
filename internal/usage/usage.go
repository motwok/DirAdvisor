// SPDX-License-Identifier: AGPL-3.0-or-later

// Package usage reports CPU, memory, disk I/O and network counters of the
// running containers. Linux reads cgroups and /proc of the host at scrape
// time; Windows polls the Docker stats API in the reconcile.
package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/ref"
)

// Docker is the part of the Docker API this package uses.
type Docker interface {
	CheckVersion(ctx context.Context) (string, bool, error)
	Containers(ctx context.Context) ([]docker.Container, error)
	Container(ctx context.Context, id string) (docker.ContainerJSON, error)
	Stats(ctx context.Context, id string) (docker.Stats, error)
}

// ErrDockerTooOld stops HatchLens.
var ErrDockerTooOld = errors.New("docker API too old")

type Options struct {
	Docker Docker
	// RootFS is where the host root filesystem is mounted (Linux).
	RootFS string
	// Windows polls the stats API instead of reading cgroups and /proc.
	Windows bool
	// NetNames maps the Windows stats network keys to Docker network names
	// (through the HatchLens Host service); nil: one sum per container.
	NetNames func(ctx context.Context) (map[string]string, error)
	Log      *slog.Logger
	Now      func() time.Time
}

type network struct {
	name, ip4, ip6 string
	prefix4        int
}

type target struct {
	id, name, stack string
	labels          map[string]string
	pid             int
	// noNet: host, none or another container's network namespace.
	noNet              bool
	networks           []network
	nanoCPUs, memLimit int64
	stats              *docker.Stats
	// netNames: Windows stats network key -> Docker network; nil: unknown.
	netNames map[string]string
}

type inspected struct {
	pid                int
	image              string
	nanoCPUs, memLimit int64
}

type Core struct {
	mu       sync.Mutex
	cfg      *config.Config
	dk       Docker
	root     string
	windows  bool
	netNames func(ctx context.Context) (map[string]string, error)
	log      *slog.Logger
	now      func() time.Time
	// selfPID is the own PID in the own PID namespace (replaced in tests).
	selfPID func() int

	versionOK  bool
	ready      bool
	recErrors  float64
	recSeconds float64
	targets    []target
	cache      map[string]inspected
	warned     map[string]bool

	// Host path of the own cgroup namespace root, for relative cgroup paths.
	baseMu   sync.Mutex
	base     string
	baseDone bool
	baseErr  error
}

func New(cfg *config.Config, o Options) *Core {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Core{cfg: cfg, dk: o.Docker, root: o.RootFS, windows: o.Windows, netNames: o.NetNames, log: o.Log, now: o.Now,
		selfPID: os.Getpid, cache: map[string]inspected{}, warned: map[string]bool{}}
}

func (c *Core) Config() *config.Config { return c.cfg }

func (c *Core) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

// Stats returns duration and failure count of the reconciles.
func (c *Core) Stats() (seconds, errs float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recSeconds, c.recErrors
}

// warnOnce logs a warning once per key.
func (c *Core) warnOnce(key, msg string, args ...any) {
	c.mu.Lock()
	seen := c.warned[key]
	c.warned[key] = true
	c.mu.Unlock()
	if !seen {
		c.log.Warn(msg, args...)
	}
}

// Reconcile lists the running containers to report. Only ErrDockerTooOld is
// returned; other errors are counted and keep the last result.
func (c *Core) Reconcile(ctx context.Context) error {
	start := c.now()
	ts, err := c.observe(ctx)
	if errors.Is(err, ErrDockerTooOld) {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			c.recErrors++
			c.log.Warn("container reconcile failed", "err", err)
		}
		return nil
	}
	c.targets = ts
	c.ready = true
	c.recSeconds = c.now().Sub(start).Seconds()
	return nil
}

func running(state string) bool {
	return state == "running" || state == "paused" || state == "restarting"
}

func stack(l map[string]string) string {
	if s := l["com.docker.compose.project"]; s != "" {
		return s
	}
	return l["com.docker.stack.namespace"]
}

func (c *Core) observe(ctx context.Context) ([]target, error) {
	if !c.versionOK {
		v, reached, err := c.dk.CheckVersion(ctx)
		if err != nil {
			if reached {
				return nil, fmt.Errorf("%w: %v", ErrDockerTooOld, err)
			}
			return nil, err
		}
		c.log.Debug("Docker API", "version", v)
		c.versionOK = true
	}
	cs, err := c.dk.Containers(ctx)
	if err != nil {
		return nil, err
	}
	var names map[string]string
	if c.windows && c.netNames != nil {
		if names, err = c.netNames(ctx); err != nil {
			c.log.Debug("network names unknown", "err", err)
		}
	}
	seen := map[string]bool{}
	var out []target
	for _, ct := range cs {
		if !running(ct.State) {
			continue
		}
		seen[ct.ID] = true
		in, err := c.inspect(ctx, ct.ID)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		images := []string{in.image}
		if !ref.IsImageID(in.image, ct.ImageID) {
			images = append(images, ref.Parse(in.image).String())
		}
		if c.cfg.ExcludedContainer(ct.Name(), ct.Labels, images...) {
			continue
		}
		t := target{id: ct.ID, name: ct.Name(), stack: stack(ct.Labels), pid: in.pid,
			nanoCPUs: in.nanoCPUs, memLimit: in.memLimit}
		t.labels = c.cfg.ExtraLabels(t.name, ct.Labels, t.stack, images, config.IsContainerOwn, func(l string) {
			c.warnOnce(t.name+"|"+l, "label skipped", "container", t.name, "label", l)
		})
		mode := ct.HostConfig.NetworkMode
		t.noNet = mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:")
		for _, name := range slices.Sorted(maps.Keys(ct.NetworkSettings.Networks)) {
			ep := ct.NetworkSettings.Networks[name]
			t.networks = append(t.networks, network{name: name,
				ip4: ep.IPAddress, prefix4: ep.IPPrefixLen, ip6: ep.GlobalIPv6Address})
		}
		if c.windows {
			st, err := c.dk.Stats(ctx, ct.ID)
			if docker.IsNotFound(err) {
				continue
			}
			if err != nil {
				c.log.Debug("container stats failed", "container", t.name, "err", err)
			} else {
				t.stats, t.netNames = &st, names
			}
		}
		out = append(out, t)
	}
	for id := range c.cache {
		if !seen[id] {
			delete(c.cache, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// inspect returns the container's PID, Config.Image and limits, cached per ID.
// A cached PID that no longer belongs to the container (restart) is read again.
func (c *Core) inspect(ctx context.Context, id string) (inspected, error) {
	if in, ok := c.cache[id]; ok && (c.windows || in.pid > 0 && c.owns(in.pid, id)) {
		return in, nil
	}
	j, err := c.dk.Container(ctx, id)
	if err != nil {
		return inspected{}, err
	}
	in := inspected{pid: j.State.Pid, image: j.Config.Image, nanoCPUs: j.HostConfig.NanoCpus, memLimit: j.HostConfig.Memory}
	c.cache[id] = in
	return in, nil
}
