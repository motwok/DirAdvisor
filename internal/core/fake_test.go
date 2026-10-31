// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/motwok/DirAdvisor/internal/config"
	"github.com/motwok/DirAdvisor/internal/docker"
	"github.com/motwok/DirAdvisor/internal/host"
	"github.com/motwok/DirAdvisor/internal/metrics"
)

type walkCall struct {
	path    string
	id      uint32
	inherit bool
	skip    map[string]bool
}

type fakeFS struct {
	mu       sync.Mutex
	dirs     map[string]bool
	projid   map[string]uint32
	mounts   map[string]host.Mount
	status   map[string]string
	usage    map[uint32]host.Usage
	usageErr error
	walks    []walkCall
	walkErr  error
	onWalk   func()
	mountErr map[string]error
	quotaErr error
	listErr  error
	idErr    map[string]error
}

func newFS() *fakeFS {
	return &fakeFS{
		dirs:   map[string]bool{"/": true},
		projid: map[string]uint32{},
		mounts: map[string]host.Mount{"/": {Point: "/", FSType: "ext4", Source: "/dev/root"}},
		status: map[string]string{},
		usage:  map[uint32]host.Usage{},
	}
}

func (f *fakeFS) mkdir(p string) {
	for p != "/" {
		f.dirs[p] = true
		p = path.Dir(p)
	}
}

func (f *fakeFS) Stat(p string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dirs[p], f.dirs[p]
}

func (f *fakeFS) ListDirs(p string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []string
	for d := range f.dirs {
		if d != "/" && path.Dir(d) == p {
			out = append(out, path.Base(d))
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeFS) MountOf(p string) (host.Mount, error) {
	if err := f.mountErr[p]; err != nil {
		return host.Mount{}, err
	}
	best := ""
	for mp := range f.mounts {
		if (mp == "/" || p == mp || strings.HasPrefix(p, mp+"/")) && len(mp) > len(best) {
			best = mp
		}
	}
	return f.mounts[best], nil
}

func (f *fakeFS) ProjectID(p string) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.dirs[p] {
		return 0, errors.New("missing")
	}
	if err := f.idErr[p]; err != nil {
		return 0, err
	}
	return f.projid[p], nil
}

func (f *fakeFS) QuotaStatus(m host.Mount) (string, error) { return f.status[m.Point], f.quotaErr }
func (f *fakeFS) ZFSVersions() (string, string)            { return "", "" }

func (f *fakeFS) Usage(_ context.Context, _ host.Mount, minID uint32) (map[uint32]host.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.usageErr != nil {
		return nil, f.usageErr
	}
	out := map[uint32]host.Usage{}
	for k, v := range f.usage {
		if k >= minID {
			out[k] = v
		}
	}
	return out, nil
}

func (f *fakeFS) Walk(_ context.Context, root string, id uint32, inherit bool, skip map[string]bool, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.walks = append(f.walks, walkCall{root, id, inherit, skip})
	if f.onWalk != nil {
		f.onWalk()
	}
	if f.walkErr != nil {
		return f.walkErr
	}
	f.projid[root] = id
	return nil
}

type fakeDocker struct {
	api        string
	down       bool
	root       string
	volumes    []docker.Volume
	containers []docker.Container
	self       docker.ContainerJSON
	digests    []string
	remote     string
	remoteErr  error
	versionErr error
	infoErr    error
	volErr     error
	ctrErr     error
}

var errDown = errors.New("down")

func (d *fakeDocker) CheckVersion(context.Context) (string, bool, error) {
	if d.down {
		return "", false, errDown
	}
	if d.versionErr != nil {
		return "", true, d.versionErr
	}
	if docker.CompareVersion(d.api, docker.MinAPIVersion) < 0 {
		return d.api, true, errors.New("too old")
	}
	return d.api, true, nil
}
func (d *fakeDocker) Info(context.Context) (docker.Info, error) {
	if d.down {
		return docker.Info{}, errDown
	}
	if d.infoErr != nil {
		return docker.Info{}, d.infoErr
	}
	return docker.Info{DockerRootDir: d.root}, nil
}
func (d *fakeDocker) Volumes(context.Context) ([]docker.Volume, error) {
	if d.down {
		return nil, errDown
	}
	if d.volErr != nil {
		return nil, d.volErr
	}
	return d.volumes, nil
}
func (d *fakeDocker) Containers(context.Context) ([]docker.Container, error) {
	if d.down {
		return nil, errDown
	}
	if d.ctrErr != nil {
		return nil, d.ctrErr
	}
	return d.containers, nil
}
func (d *fakeDocker) Container(_ context.Context, id string) (docker.ContainerJSON, error) {
	if id != d.self.ID {
		return docker.ContainerJSON{}, errors.New("no such container")
	}
	return d.self, nil
}
func (d *fakeDocker) ImageRepoDigests(context.Context, string) ([]string, error) {
	return d.digests, nil
}
func (d *fakeDocker) DistributionDigest(context.Context, string) (string, error) {
	return d.remote, d.remoteErr
}

type env struct {
	t   *testing.T
	fs  *fakeFS
	dk  *fakeDocker
	cfg *config.Config
	dir string
	now time.Time
	c   *Core
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, fs: newFS(), dk: &fakeDocker{api: "1.47", root: "/var/lib/docker"}, cfg: config.Default(),
		dir: t.TempDir(), now: time.Unix(1_700_000_000, 0)}
	e.start()
	return e
}

func (e *env) start() {
	e.c = New(e.cfg, Options{FS: e.fs, Docker: e.dk, StorageDir: e.dir, SelfID: "self0000000000",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return e.now }})
}

func (e *env) volume(name string, labels, opts map[string]string) {
	mp := "/var/lib/docker/volumes/" + name + "/_data"
	e.fs.mkdir(mp)
	if opts["device"] != "" {
		e.fs.mkdir(opts["device"])
	}
	e.dk.volumes = append(e.dk.volumes, docker.Volume{Name: name, Driver: "local", Mountpoint: mp, Labels: labels,
		Options: opts, CreatedAt: "2026-01-0" + string(rune('1'+len(e.dk.volumes))) + "T00:00:00Z"})
}

func (e *env) container(id, name, state string, created int64, labels map[string]string, mounts ...docker.Mount) {
	e.dk.containers = append(e.dk.containers, docker.Container{ID: id, Names: []string{"/" + name}, State: state,
		Created: created, Labels: labels, Mounts: mounts})
}

func hostMount(p, fstype, src string) host.Mount {
	return host.Mount{Point: p, FSType: fstype, Source: src}
}

func rw(vol string) docker.Mount { return docker.Mount{Type: "volume", Name: vol, RW: true} }
func ro(vol string) docker.Mount { return docker.Mount{Type: "volume", Name: vol} }

func (e *env) reconcile() {
	e.t.Helper()
	if err := e.c.Reconcile(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// walkAll runs all pending walks synchronously.
func (e *env) walkAll() {
	for i := 0; i < 100; i++ {
		j, ok := e.c.nextJob()
		if !ok {
			return
		}
		err := e.fs.Walk(context.Background(), j.path, j.id, j.inherit, j.skip, j.rate)
		e.c.finishJob(j, err)
	}
	e.t.Fatal("walks do not end")
}

func (e *env) entry(p string) *entryView {
	for i := range e.c.view.entries {
		if e.c.view.entries[i].Path == p {
			return &e.c.view.entries[i]
		}
	}
	return nil
}

func (e *env) reason(p string) string {
	for _, u := range e.c.view.unmeas {
		if u.Path == p {
			return u.Reason
		}
	}
	return ""
}

func (e *env) metricsText() string {
	var b strings.Builder
	metrics.Write(&b, e.c.Collect(context.Background(), BuildInfo{Version: "1.2.3", Revision: "abc", GoVersion: "go1.27"}))
	return b.String()
}
