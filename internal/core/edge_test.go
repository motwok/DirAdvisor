// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/motwok/DirAdvisor/internal/config"
	"github.com/motwok/DirAdvisor/internal/docker"
	"github.com/motwok/DirAdvisor/internal/host"
	"github.com/motwok/DirAdvisor/internal/state"
)

func TestDockerReadErrors(t *testing.T) {
	boom := errors.New("boom")
	for name, set := range map[string]func(*fakeDocker){
		"version":    func(d *fakeDocker) { d.versionErr = boom },
		"info":       func(d *fakeDocker) { d.infoErr = boom },
		"volumes":    func(d *fakeDocker) { d.volErr = boom },
		"containers": func(d *fakeDocker) { d.ctrErr = boom },
	} {
		e := newEnv(t)
		set(e.dk)
		e.reconcile()
		if e.c.Ready() || e.c.recErrors != 1 {
			t.Errorf("%s: ready=%v errors=%v", name, e.c.Ready(), e.c.recErrors)
		}
	}
}

func TestDefaultDockerRoot(t *testing.T) {
	e := newEnv(t)
	e.dk.root = ""
	e.reconcile()
	if e.c.DockerRoot() != defaultDockerRoot {
		t.Fatalf("root %q", e.c.DockerRoot())
	}
}

func TestMountAndQuotaErrors(t *testing.T) {
	e := newEnv(t)
	e.fs.mkdir("/srv/a")
	e.fs.mkdir("/srv/b")
	e.cfg.Directories = []config.Directory{{Path: "/srv/a"}, {Path: "/srv/b"}}
	e.fs.mountErr = map[string]error{"/srv/a": errors.New("no mount")}
	e.fs.idErr = map[string]error{"/srv/b": errors.New("eio")}
	e.reconcile()
	if e.entry("/srv/a") != nil || e.reason("/srv/a") != "" || e.reason("/srv/b") != reasonPathMissing {
		t.Fatalf("entries %+v unmeas %+v", e.c.view.entries, e.c.view.unmeas)
	}
	e.fs.quotaErr = errors.New("quotactl")
	e.reconcile()
	if len(e.c.view.entries) != 0 {
		t.Fatalf("quota error: %+v", e.c.view.entries)
	}
}

func TestActionsDeduplicatedAndThrottled(t *testing.T) {
	e := newEnv(t)
	e.volume("a", nil, nil)
	e.volume("b", nil, nil)
	e.fs.status["/"] = host.EnableProjectQuotaFeature
	e.reconcile()
	e.reconcile()
	if len(e.c.view.actions) != 1 || len(e.c.logged) != 1 {
		t.Fatalf("actions %+v", e.c.view.actions)
	}
}

func TestOwnerWorkingDirAndLastState(t *testing.T) {
	e := newEnv(t)
	e.volume("v", nil, nil)
	e.dk.containers = []docker.Container{ctr("1", "app-db-1", "running", 1, "app", rw("v"))}
	e.reconcile()
	if e.c.st.Entries[state.VolumeKey("v")].WorkingDir != "/opt/app" {
		t.Fatalf("working dir: %+v", e.c.st.Entries[state.VolumeKey("v")])
	}
	e.dk.down = true
	e.reconcile()
	if ev := e.entry(volPathOf("v")); ev == nil || ev.State != StateRunning {
		t.Fatalf("last state: %+v", ev)
	}
}

func TestVolumeKindFallbacks(t *testing.T) {
	e := newEnv(t)
	e.volume("tmp", nil, map[string]string{"type": "tmpfs"})
	e.volume("nobind", nil, map[string]string{"type": "none", "device": "/srv/x"})
	e.dk.volumes = append(e.dk.volumes, docker.Volume{Name: "out", Driver: "local", Mountpoint: "/elsewhere/out"})
	e.reconcile()
	for _, p := range []string{volPathOf("tmp"), volPathOf("nobind"), "/elsewhere/out"} {
		if e.reason(p) != reasonUnsupportedDrv {
			t.Errorf("%s: %q", p, e.reason(p))
		}
	}
}

func TestExcludedDirectoriesAndGlobErrors(t *testing.T) {
	e := newEnv(t)
	e.fs.mkdir("/var/lib/docker/x")
	e.fs.mkdir("/var/lib/other")
	e.cfg.Directories = []config.Directory{{Path: "/var/lib/docker/x"}, {Path: "/var/lib/*"}}
	e.reconcile()
	if e.reason("/var/lib/docker/x") != reasonExcludedPath || e.entry("/var/lib/other") == nil || e.entry("/var/lib/docker") != nil {
		t.Fatalf("entries %+v unmeas %+v", e.c.view.entries, e.c.view.unmeas)
	}
	e.fs.listErr = errors.New("eacces")
	e.reconcile()
	if e.reason("/var/lib") != reasonPathMissing || e.c.recErrors != 1 {
		t.Fatalf("glob error: %+v", e.c.view.unmeas)
	}
}

func TestDuplicateConfigPathsKeepOrder(t *testing.T) {
	e := newEnv(t)
	e.fs.mkdir("/srv/x")
	e.cfg.Directories = []config.Directory{{Path: "/srv/*"}, {Path: "/srv/x"}}
	e.reconcile()
	if ev := e.entry("/srv/x"); ev == nil || ev.Key != state.ConfigKey("/srv/x") || e.reason("/srv/x") != reasonDuplicatePath {
		t.Fatalf("entries %+v unmeas %+v", e.c.view.entries, e.c.view.unmeas)
	}
}

func TestRemovedMissingDirectoryAndReAdd(t *testing.T) {
	e := newEnv(t)
	e.fs.mkdir("/srv/a")
	e.fs.mkdir("/srv/b")
	e.cfg.Directories = []config.Directory{{Path: "/srv/a"}, {Path: "/srv/b"}}
	e.reconcile()
	e.walkAll()

	// a: removed and gone, waits for a reset until it shows up again.
	delete(e.fs.dirs, "/srv/a")
	// b: removed while present, then added back before the reset ran.
	e.cfg.Directories = nil
	e.reconcile()
	ea, eb := e.c.st.Entries[state.ConfigKey("/srv/a")], e.c.st.Entries[state.ConfigKey("/srv/b")]
	if !ea.PendingReset || !eb.PendingReset {
		t.Fatalf("pending: %+v %+v", ea, eb)
	}
	e.cfg.Directories = []config.Directory{{Path: "/srv/b"}}
	e.reconcile()
	if eb.PendingReset || eb.WalkDone {
		t.Fatalf("re-added: %+v", eb)
	}
	if j, ok := e.c.nextJob(); !ok || j.reset || j.path != "/srv/b" {
		t.Fatalf("job %+v %v", j, ok)
	}
}

func TestCollectEdges(t *testing.T) {
	e := newEnv(t)
	e.volume("b", nil, nil)
	e.volume("a", nil, nil)
	e.fs.mkdir("/srv/o/i")
	e.cfg.Directories = []config.Directory{{Path: "/srv/o", Labels: map[string]string{"team": "ops"}}, {Path: "/srv/o/i"}}
	e.reconcile()
	if e.entry(volPathOf("a")).ProjectID <= e.entry(volPathOf("b")).ProjectID {
		t.Fatal("test needs a lower ID on the later path")
	}
	txt := e.metricsText()
	if !strings.Contains(txt, `team="ops"`) || !strings.Contains(txt, `inner="/srv/o/i"`) {
		t.Fatalf("metrics:\n%s", txt)
	}
	e.fs.usageErr = errors.New("eperm")
	if strings.Contains(e.metricsText(), "directory_bytes{") {
		t.Fatal("no byte samples without usage")
	}
}

func TestNewDefaultsAndStateErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The storage dir is a file: loading and saving the state fail.
	c := New(config.Default(), Options{FS: newFS(), StorageDir: file})
	if c.log == nil || c.now == nil {
		t.Fatal("defaults")
	}
	c.st.Entries["config:/x"] = &state.Entry{Path: "/x"}
	c.Save()
	if _, err := os.Stat(filepath.Join(file, state.FileName)); err == nil {
		t.Fatal("state written")
	}
}

func TestImageLoopRepeats(t *testing.T) {
	e := newEnv(t)
	e.cfg.Global.ImageUpdateCheck.Interval = config.Duration(time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	e.c.RunImageCheck(ctx)
}

func TestWalkerStopsOnCancel(t *testing.T) {
	e := newEnv(t)
	e.volume("v", nil, nil)
	e.reconcile()
	ctx, cancel := context.WithCancel(context.Background())
	e.fs.onWalk = cancel
	e.c.RunWalker(ctx)
	if e.c.st.Entries[state.VolumeKey("v")].WalkDone {
		t.Fatal("cancelled walk must not finish")
	}
}

func TestWalkerSkipsAndVanishedEntry(t *testing.T) {
	e := newEnv(t)
	e.fs.mkdir("/var/x")
	e.cfg.Directories = []config.Directory{{Path: "/var"}}
	e.reconcile()
	j, ok := e.c.nextJob()
	if !ok || !j.skip["/var/lib/docker"] {
		t.Fatalf("job %+v", j)
	}
	delete(e.c.st.Entries, j.key)
	e.c.finishJob(j, nil)
}

func TestLabelRuleMismatch(t *testing.T) {
	cfg := config.Default()
	cfg.Docker.ContainerLabels = []config.LabelRef{{From: "a", To: "renamed"}}
	cfg.Docker.Labels = []config.LabelRule{
		{Match: config.Match{Stack: "other"}, Labels: map[string]string{"x": "1"}},
		{Match: config.Match{Path: "/srv/*"}, Labels: map[string]string{"y": "1"}},
	}
	o := ownership{Stack: "app"}
	l := volumeLabels(cfg, docker.Volume{Name: "v"}, volPath, o, map[string]string{"renamed": "keep", "plain": "drop"})
	if l["renamed"] != "keep" || l["plain"] != "" || l["x"] != "" || l["y"] != "" {
		t.Fatalf("%v", l)
	}
}

func TestOwnerEdges(t *testing.T) {
	vol := docker.Volume{Name: "v"}
	bind := docker.Mount{Type: "bind", Name: ""}
	cs := []docker.Container{
		ctr("1", "gone", "exited", 1, "", bind),
		ctr("2", "b-1", "running", 5, "", rw("v")),
		ctr("3", "a-1", "running", 5, "", rw("v")),
	}
	if o := resolveOwner(vol, cs[:1], &state.Entry{Owner: "gone"}, ""); o.OwnerC == nil || o.State != StateStopped {
		t.Fatalf("remembered owner: %+v", o)
	}
	if o := resolveOwner(vol, cs, nil, ""); o.Owner != "a-1" {
		t.Fatalf("tie by name: %+v", o)
	}
}

func TestWalkerPolls(t *testing.T) {
	old := walkerPoll
	walkerPoll = time.Millisecond
	t.Cleanup(func() { walkerPoll = old })
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	e.c.RunWalker(ctx)
}
