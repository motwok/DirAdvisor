// SPDX-License-Identifier: AGPL-3.0-or-later

package images

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/metrics"
)

type fakeDocker struct {
	mu         sync.Mutex
	version    string
	reached    bool
	verErr     error
	containers []docker.Container
	listErr    error
	configs    map[string]string
	inspectErr error
	gone       map[string]bool
	images     map[string]docker.Image
	imageErr   map[string]error
	dist       map[string]string
	distErr    map[string]error
	auths      []string
	distRefs   []string
	onDist     func()
}

func newFake() *fakeDocker {
	return &fakeDocker{version: "1.47", reached: true, configs: map[string]string{}, images: map[string]docker.Image{},
		imageErr: map[string]error{}, dist: map[string]string{}, distErr: map[string]error{}, gone: map[string]bool{}}
}

func (f *fakeDocker) CheckVersion(context.Context) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, f.reached, f.verErr
}

func (f *fakeDocker) Containers(context.Context) ([]docker.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]docker.Container(nil), f.containers...), f.listErr
}

func (f *fakeDocker) Container(_ context.Context, id string) (docker.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone[id] {
		return docker.ContainerJSON{}, &docker.APIError{Status: 404, Message: "no such container"}
	}
	var j docker.ContainerJSON
	j.ID = id
	j.Config.Image = f.configs[id]
	return j, f.inspectErr
}

func (f *fakeDocker) Image(_ context.Context, s string) (docker.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.imageErr[s]; err != nil {
		return docker.Image{}, err
	}
	im, ok := f.images[s]
	if !ok {
		return docker.Image{}, &docker.APIError{Status: 404, Message: "no such image"}
	}
	return im, nil
}

func (f *fakeDocker) Distribution(_ context.Context, r, auth string) (string, error) {
	f.mu.Lock()
	f.auths = append(f.auths, auth)
	f.distRefs = append(f.distRefs, r)
	d, err, hook := f.dist[r], f.distErr[r], f.onDist
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return d, err
}

// add registers a container whose image ID is id and Config.Image is image.
func (f *fakeDocker) add(name, image, id, st string, labels map[string]string, repoDigests ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cid := "c-" + name
	f.containers = append(f.containers, docker.Container{ID: cid, Names: []string{"/" + name}, ImageID: id,
		State: st, Labels: labels})
	f.configs[cid] = image
	f.images[id] = docker.Image{ID: id, RepoDigests: repoDigests}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type rig struct {
	t   *testing.T
	f   *fakeDocker
	clk *clock
	c   *Core
	dir string
	log *bytes.Buffer
	cfg *config.Config
}

func newRig(t *testing.T, cfgText string) *rig {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgText), func(string) (string, bool) { return "", false }, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, f: newFake(), clk: &clock{t: time.Unix(1_000_000, 0)}, dir: t.TempDir(), log: &bytes.Buffer{}, cfg: cfg}
	r.start()
	return r
}

// start creates a new core on the same storage, like a restart.
func (r *rig) start() {
	r.c = New(r.cfg, Options{Docker: r.f, StorageDir: r.dir, Now: r.clk.now,
		Log: slog.New(slog.NewTextHandler(r.log, nil)), Rand: func(n int64) int64 { return n / 2 }})
}

func (r *rig) reconcile() {
	r.t.Helper()
	if err := r.c.Reconcile(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

// checkAll runs every check that is due now.
func (r *rig) checkAll() {
	for i := 0; i < 50; i++ {
		if r.c.CheckDue(context.Background()) != 0 {
			return
		}
	}
	r.t.Fatal("checks do not end")
}

func (r *rig) metrics() string {
	var b strings.Builder
	metrics.Write(&b, r.c.Collect())
	return b.String()
}

func (r *rig) has(lines ...string) {
	r.t.Helper()
	out := r.metrics()
	for _, l := range lines {
		if !strings.Contains(out, l+"\n") {
			r.t.Errorf("missing %q in\n%s", l, out)
		}
	}
}

func (r *rig) hasNot(parts ...string) {
	r.t.Helper()
	out := r.metrics()
	for _, p := range parts {
		if strings.Contains(out, p) {
			r.t.Errorf("unexpected %q in\n%s", p, out)
		}
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var errBoom = errors.New("boom")
