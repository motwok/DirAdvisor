// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"testing"
	"time"

	"github.com/motwok/DirAdvisor/internal/config"
	"github.com/motwok/DirAdvisor/internal/state"
)

// A glob match and an explicit path for the same directory share one state
// entry; dedupe must not delete it (DIS-9a).
func TestGlobAndExplicitSamePath(t *testing.T) {
	e := newEnv(t)
	e.fs.mkdir("/srv/x")
	e.cfg.Directories = []config.Directory{{Path: "/srv/*"}, {Path: "/srv/x"}}
	for i := 0; i < 3; i++ {
		e.reconcile()
		e.walkAll()
	}
	en := e.c.st.Entries[state.ConfigKey("/srv/x")]
	if len(e.fs.walks) != 1 || en == nil || !en.WalkDone || e.reason("/srv/x") != reasonDuplicatePath {
		t.Fatalf("walks %d entry %+v reason %q", len(e.fs.walks), en, e.reason("/srv/x"))
	}
}

// A volume re-created under the same name but another path does not take
// the pending reset's ID; the old path is still reset (PID-8, PID-12).
func TestPendingResetKeepsPathOnReuse(t *testing.T) {
	e := newEnv(t)
	e.volume("b", nil, map[string]string{"type": "none", "o": "bind", "device": "/srv/b"})
	e.reconcile()
	e.walkAll()
	e.dk.volumes = nil
	e.reconcile()
	e.volume("b", nil, nil)
	e.reconcile()
	rk := e.c.st.Entries[state.ResetKey("/srv/b")]
	nb := e.c.st.Entries[state.VolumeKey("b")]
	if rk == nil || !rk.PendingReset || rk.ProjectID != 100000 || nb.ProjectID != 100001 || nb.Path != volPathOf("b") {
		t.Fatalf("reset %+v new %+v", rk, nb)
	}
	e.walkAll()
	if e.fs.projid["/srv/b"] != 0 || e.fs.projid[volPathOf("b")] != 100001 || e.c.st.Entries[state.ResetKey("/srv/b")] != nil {
		t.Fatalf("ids %v entries %v", e.fs.projid, e.c.st.Entries)
	}
	// A second same-path reset keeps the first one.
	e.c.st.Entries[state.ResetKey("/srv/c")] = &state.Entry{Path: "/srv/c", ProjectID: 5, PendingReset: true}
	e.c.st.Entries[state.VolumeKey("c")] = &state.Entry{Source: state.SourceVolume, Path: "/srv/c", ProjectID: 6, PendingReset: true}
	e.volume("c", nil, nil)
	e.reconcile()
	if e.c.st.Entries[state.ResetKey("/srv/c")].ProjectID != 5 || e.c.st.Entries[state.VolumeKey("c")].ProjectID == 6 {
		t.Fatalf("entries %v", e.c.st.Entries)
	}
}

// An unmeasurable inner entry (foreign ID) is not in the state but must not
// be entered by the outer walk (PID-3, PID-7).
func TestWalkSkipsUnmeasurableInnerEntry(t *testing.T) {
	e := newEnv(t)
	e.fs.mkdir("/srv/x")
	e.fs.projid["/srv/x"] = 5
	e.cfg.Directories = []config.Directory{{Path: "/srv"}, {Path: "/srv/x"}}
	e.reconcile()
	j, ok := e.c.nextJob()
	if !ok || j.path != "/srv" || !j.skip["/srv/x"] || e.reason("/srv/x") != reasonForeignID {
		t.Fatalf("job %+v %v reason %q", j, ok, e.reason("/srv/x"))
	}
}

// Before Docker was ever reached its root directory is unknown, so configured
// directories wait; the root is kept in the state for later starts (DIS-8).
func TestDockerRootUnknownAtStart(t *testing.T) {
	e := newEnv(t)
	e.dk.root = "/data/docker"
	e.dk.down = true
	e.fs.mkdir("/data/docker/overlay2")
	e.cfg.Directories = []config.Directory{{Path: "/data"}}
	e.reconcile()
	if e.reason("/data") != reasonRootUnknown || e.c.st.Entries[state.ConfigKey("/data")] != nil {
		t.Fatalf("reason %q entries %v", e.reason("/data"), e.c.st.Entries)
	}
	e.c.st.Entries["x"] = &state.Entry{Source: state.SourceConfig, Path: "/data/x", ProjectID: 9, PendingReset: true}
	e.fs.mkdir("/data/x")
	if _, ok := e.c.nextJob(); ok {
		t.Fatal("no walk before the root is known")
	}
	delete(e.c.st.Entries, "x")
	e.dk.down = false
	e.reconcile()
	j, ok := e.c.nextJob()
	if !ok || !j.skip["/data/docker"] || e.c.st.DockerRoot != "/data/docker" {
		t.Fatalf("job %+v %v root %q", j, ok, e.c.st.DockerRoot)
	}
	e.c.Save()
	// Restart with Docker down: the root comes from the state.
	e.dk.down = true
	e.start()
	e.reconcile()
	if j, ok := e.c.nextJob(); !ok || !j.skip["/data/docker"] {
		t.Fatalf("job after restart %+v %v", j, ok)
	}
}

// Two globs with the same missing prefix give one unmeasurable series.
func TestUnmeasurableSeriesUnique(t *testing.T) {
	e := newEnv(t)
	e.cfg.Directories = []config.Directory{{Path: "/data/*/a"}, {Path: "/data/*/b"}}
	e.reconcile()
	if len(e.c.view.unmeas) != 1 || e.c.view.unmeas[0] != (unmeasurable{"/data", reasonPathMissing}) {
		t.Fatalf("unmeas %+v", e.c.view.unmeas)
	}
}

// Without a Docker client (--docker.socket="") the default config is still ready.
func TestReadyWithoutDockerClient(t *testing.T) {
	e := newEnv(t)
	e.c = New(e.cfg, Options{FS: e.fs, StorageDir: e.dir, Log: e.c.log, Now: func() time.Time { return e.now }})
	e.reconcile()
	if !e.c.Ready() {
		t.Fatal("not ready")
	}
}

// Labels from the owner container survive the container (LBL-4).
func TestContainerLabelsKeptWithoutOwner(t *testing.T) {
	e := newEnv(t)
	e.cfg.Docker.ContainerLabels = []config.LabelRef{{From: "com.example.team"}}
	e.volume("v", map[string]string{"com.docker.compose.project": "app"}, nil)
	e.container("c1", "app-web-1", "running", 1, map[string]string{
		"com.docker.compose.project": "app", "com.example.team": "ops", "diradvisor.label.tier": "hot",
	}, rw("v"))
	e.reconcile()
	want := map[string]string{"container_label_com_example_team": "ops", "tier": "hot"}
	check := func(when string) {
		t.Helper()
		l := e.entry(volPathOf("v")).Labels
		for k, v := range want {
			if l[k] != v {
				t.Fatalf("%s: %s = %q (all %v)", when, k, l[k], l)
			}
		}
	}
	check("with owner")
	e.dk.containers = nil
	e.reconcile()
	check("compose down")
	// A state from before container labels were stored separately.
	en := e.c.st.Entries[state.VolumeKey("v")]
	en.ContainerLabels = nil
	e.reconcile()
	if l := e.entry(volPathOf("v")).Labels; l["container_label_com_example_team"] != "ops" || l["tier"] != "" {
		t.Fatalf("migrated: %v", l)
	}
}
