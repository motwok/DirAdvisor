// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/host"
	"github.com/motwok/HatchLens/internal/ref"
	"github.com/motwok/HatchLens/internal/state"
)

const volPath = "/var/lib/docker/volumes/app_data/_data"

func TestNewVolumeGetsIDAndWalk(t *testing.T) {
	e := newEnv(t)
	e.volume("app_data", map[string]string{composeProject: "app"}, nil)
	e.container("c1", "app-web-1", "running", 10, map[string]string{composeProject: "app"}, rw("app_data"))
	e.reconcile()
	v := e.entry(volPath)
	if v == nil || v.ProjectID != 100000 || !v.Assigning || v.Owner != "app-web-1" || v.Stack != "app" || v.State != StateRunning {
		t.Fatalf("view: %+v", v)
	}
	if !e.c.Ready() {
		t.Fatal("not ready")
	}
	e.walkAll()
	if len(e.fs.walks) != 1 || e.fs.walks[0].id != 100000 || !e.fs.walks[0].inherit {
		t.Fatalf("walks: %+v", e.fs.walks)
	}
	if e.c.view.entries[0].Assigning {
		t.Fatal("still assigning")
	}
	e.fs.usage[100000] = host.Usage{Bytes: 4096, Inodes: 2}
	out := e.metricsText()
	for _, want := range []string{
		`hatchlens_directory_bytes{path="` + volPath + `",source="volume",stack="app",volume="app_data"} 4096`,
		`hatchlens_directory_inodes{path="` + volPath + `",source="volume",stack="app",volume="app_data"} 2`,
		`hatchlens_directory_info{owner="app-web-1",path="` + volPath + `",project_id="100000",state="running"} 1`,
		`hatchlens_directory_last_used_timestamp_seconds{path="` + volPath + `"} 1.7e+09`,
		`hatchlens_directory_assigning{path="` + volPath + `"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	// Restart: state keeps the ID, no new walk.
	e.start()
	e.reconcile()
	e.walkAll()
	if len(e.fs.walks) != 1 || e.entry(volPath).ProjectID != 100000 {
		t.Fatalf("after restart: %+v", e.fs.walks)
	}
}

func TestAllocationSkipsUsedIDs(t *testing.T) {
	e := newEnv(t)
	e.fs.usage[100000] = host.Usage{Bytes: 1}
	e.volume("a", nil, nil)
	e.volume("b", nil, nil)
	e.reconcile()
	got := []uint32{e.entry("/var/lib/docker/volumes/a/_data").ProjectID, e.entry("/var/lib/docker/volumes/b/_data").ProjectID}
	if got[0] != 100001 || got[1] != 100002 {
		t.Fatalf("ids %v", got)
	}
}

func TestExhaustedAndUsageError(t *testing.T) {
	e := newEnv(t)
	e.cfg.Global.ProjectIDs = config.IDRange{From: 10, To: 11}
	e.volume("a", nil, nil)
	e.volume("b", nil, nil)
	e.volume("c", nil, nil)
	e.reconcile()
	if e.reason("/var/lib/docker/volumes/c/_data") != reasonExhausted {
		t.Fatalf("unmeas: %+v", e.c.view.unmeas)
	}
	e2 := newEnv(t)
	e2.fs.usageErr = errors.New("eperm")
	e2.volume("a", nil, nil)
	e2.reconcile()
	if e2.reason("/var/lib/docker/volumes/a/_data") != reasonQuotaAccess {
		t.Fatalf("usage error: %+v", e2.c.view.unmeas)
	}
	if strings.Contains(e2.metricsText(), "directory_bytes") {
		t.Fatal("no bytes expected")
	}
}

func TestVolumeKinds(t *testing.T) {
	e := newEnv(t)
	e.volume("nfs", nil, map[string]string{"type": "nfs"})
	e.volume("bind", nil, map[string]string{"type": "none", "o": "bind", "device": "/srv/bind"})
	e.volume("bad", nil, map[string]string{"type": "none", "o": "bind", "device": "/var/lib/docker/x"})
	e.volume("buildx_cache", nil, nil)
	e.dk.volumes = append(e.dk.volumes, docker.Volume{Name: "plug", Driver: "rexray", Mountpoint: "/mnt/plug"})
	e.cfg.Exclude.Volumes = []string{"buildx_*"}
	e.reconcile()
	if e.reason("/var/lib/docker/volumes/nfs/_data") != host.RemoteFilesystem ||
		e.reason("/var/lib/docker/x") != reasonExcludedPath || e.reason("/mnt/plug") != reasonUnsupportedDrv {
		t.Fatalf("unmeas: %+v", e.c.view.unmeas)
	}
	if e.entry("/srv/bind") == nil || e.entry("/var/lib/docker/volumes/buildx_cache/_data") != nil {
		t.Fatalf("entries: %+v", e.c.view.entries)
	}
	if strings.Contains(e.metricsText(), "buildx") {
		t.Fatal("excluded volume must not appear")
	}
}

func TestAdminActionAndForeignID(t *testing.T) {
	e := newEnv(t)
	e.fs.mounts["/srv"] = host.Mount{Point: "/srv", FSType: "ext4", Source: "/dev/sdb1"}
	e.fs.status["/srv"] = host.EnableProjectQuotaFeature
	e.fs.mkdir("/srv/a")
	e.fs.mkdir("/opt/b")
	e.fs.projid["/opt/b"] = 42
	e.cfg.Directories = []config.Directory{{Path: "/srv/a"}, {Path: "/opt/b"}}
	e.reconcile()
	if e.reason("/srv/a") != host.EnableProjectQuotaFeature || e.reason("/opt/b") != reasonForeignID {
		t.Fatalf("unmeas: %+v", e.c.view.unmeas)
	}
	out := e.metricsText()
	if !strings.Contains(out, `hatchlens_admin_action_needed{action="enable_project_quota_feature",fstype="ext4",mountpoint="/srv"} 1`) {
		t.Fatalf("action missing:\n%s", out)
	}
	if !strings.Contains(e.c.view.actions[0].Command, "tune2fs -O project,quota /dev/sdb1") {
		t.Fatalf("command: %q", e.c.view.actions[0].Command)
	}
	e.fs.status["/srv"] = ""
	e.reconcile()
	if len(e.c.view.actions) != 0 || len(e.c.logged) != 0 {
		t.Fatal("action must disappear")
	}
}

func TestConfiguredAndGlob(t *testing.T) {
	e := newEnv(t)
	e.fs.mkdir("/home/alice")
	e.fs.mkdir("/home/bob")
	e.fs.mkdir("/srv/backup")
	e.cfg.Directories = []config.Directory{
		{Path: "/srv/backup", Labels: map[string]string{"stack": "backup", "owner": "borg", "tier": "cold"}},
		{Path: "/home/*", Labels: map[string]string{"user": "$1"}},
		{Path: "/missing/*"},
		{Path: "/gone"},
	}
	e.reconcile()
	b := e.entry("/srv/backup")
	if b == nil || b.Stack != "backup" || b.Owner != "borg" || b.Labels["tier"] != "cold" || b.State != "" {
		t.Fatalf("backup: %+v", b)
	}
	if a := e.entry("/home/alice"); a == nil || a.Labels["user"] != "alice" {
		t.Fatalf("alice: %+v", a)
	}
	if e.reason("/missing") != reasonPathMissing || e.reason("/gone") != reasonPathMissing {
		t.Fatalf("unmeas: %+v", e.c.view.unmeas)
	}
	e.walkAll()
	// A vanished glob match is released without reset.
	delete(e.fs.dirs, "/home/bob")
	e.reconcile()
	if _, ok := e.c.st.Entries[state.ConfigKey("/home/bob")]; ok {
		t.Fatal("bob must be released")
	}
	// Parent missing: nothing released.
	delete(e.fs.dirs, "/home/alice")
	delete(e.fs.dirs, "/home")
	e.reconcile()
	if _, ok := e.c.st.Entries[state.ConfigKey("/home/alice")]; !ok {
		t.Fatal("alice must be kept while /home is missing")
	}
}

func TestRemovedDirectoryIsResetToEnclosing(t *testing.T) {
	e := newEnv(t)
	e.fs.mkdir("/srv/inner")
	e.cfg.Directories = []config.Directory{{Path: "/srv"}, {Path: "/srv/inner"}}
	e.reconcile()
	e.walkAll()
	outer, inner := e.entry("/srv").ProjectID, e.entry("/srv/inner").ProjectID
	if len(e.c.view.overlap) != 1 || e.c.view.overlap[0] != (overlap{"/srv", "/srv/inner"}) {
		t.Fatalf("overlap: %+v", e.c.view.overlap)
	}
	for _, w := range e.fs.walks {
		if w.path == "/srv" && !w.skip["/srv/inner"] {
			t.Fatalf("outer walk must skip inner: %+v", w)
		}
	}
	e.cfg.Directories = []config.Directory{{Path: "/srv"}}
	e.reconcile()
	se := e.c.st.Entries[state.ConfigKey("/srv/inner")]
	if se == nil || !se.PendingReset || se.ResetTo != outer {
		t.Fatalf("pending reset: %+v", se)
	}
	e.walkAll()
	if _, ok := e.c.st.Entries[state.ConfigKey("/srv/inner")]; ok || e.fs.projid["/srv/inner"] != outer {
		t.Fatalf("reset not done: %v", e.fs.projid["/srv/inner"])
	}
	_ = inner
	// Pending reset survives a restart.
	e.cfg.Directories = nil
	e.reconcile()
	e.start()
	if se := e.c.st.Entries[state.ConfigKey("/srv")]; se == nil || !se.PendingReset {
		t.Fatal("pending reset lost")
	}
	e.walkAll()
	if len(e.c.st.Entries) != 0 || e.fs.projid["/srv"] != 0 {
		t.Fatalf("entries: %+v", e.c.st.Entries)
	}
}

func TestVolumeReleaseAndExclude(t *testing.T) {
	e := newEnv(t)
	e.volume("plain", nil, nil)
	e.volume("bind", nil, map[string]string{"type": "none", "o": "bind", "device": "/srv/b"})
	e.volume("later", nil, nil)
	e.reconcile()
	e.walkAll()
	e.dk.volumes = e.dk.volumes[2:]
	delete(e.fs.dirs, "/var/lib/docker/volumes/plain/_data")
	e.cfg.Exclude.Volumes = []string{"later"}
	e.reconcile()
	if _, ok := e.c.st.Entries[state.VolumeKey("plain")]; ok {
		t.Fatal("plain must be released")
	}
	if !e.c.st.Entries[state.VolumeKey("bind")].PendingReset || !e.c.st.Entries[state.VolumeKey("later")].PendingReset {
		t.Fatal("bind and excluded must be reset first")
	}
	e.walkAll()
	if len(e.c.st.Entries) != 0 || e.fs.projid["/srv/b"] != 0 {
		t.Fatalf("entries: %+v", e.c.st.Entries)
	}
}

func TestDockerDisabledKeepsIDs(t *testing.T) {
	e := newEnv(t)
	e.volume("v", nil, nil)
	e.reconcile()
	e.walkAll()
	e.cfg.Disable = []string{config.FnVolume}
	e.reconcile()
	if len(e.c.view.entries) != 0 || e.c.st.Entries[state.VolumeKey("v")].PendingReset {
		t.Fatal("volume must be kept silently")
	}
	e.cfg.Disable = nil
	e.reconcile()
	e.walkAll()
	if len(e.fs.walks) != 1 {
		t.Fatalf("no new walk expected: %+v", e.fs.walks)
	}
}

func TestDockerDownAndTooOld(t *testing.T) {
	e := newEnv(t)
	e.volume("v", nil, nil)
	e.fs.mkdir("/srv/x")
	e.cfg.Directories = []config.Directory{{Path: "/srv/x"}}
	e.reconcile()
	e.walkAll()
	e.dk.down = true
	e.start()
	e.reconcile()
	if e.c.Ready() || e.entry("/srv/x") == nil || e.entry(volPathOf("v")) == nil || e.c.recErrors != 1 {
		t.Fatalf("down: ready=%v entries=%+v", e.c.Ready(), e.c.view.entries)
	}
	e.dk.down = false
	e.reconcile()
	if !e.c.Ready() {
		t.Fatal("ready after recovery")
	}
	old := newEnv(t)
	old.dk.api = "1.40"
	if err := old.c.Reconcile(context.Background()); !errors.Is(err, ErrDockerTooOld) {
		t.Fatalf("expected too old, got %v", err)
	}
}

func volPathOf(n string) string { return "/var/lib/docker/volumes/" + n + "/_data" }

func TestLostStateAdoptsID(t *testing.T) {
	e := newEnv(t)
	e.volume("v", nil, nil)
	e.fs.projid[volPathOf("v")] = 100005
	e.reconcile()
	if e.entry(volPathOf("v")).ProjectID != 100005 {
		t.Fatalf("not adopted: %+v", e.entry(volPathOf("v")))
	}
	e.walkAll()
	if len(e.fs.walks) != 1 || e.fs.walks[0].id != 100005 {
		t.Fatal("walk must be repeated with the adopted ID")
	}
}

func TestDuplicatePaths(t *testing.T) {
	e := newEnv(t)
	e.fs.mkdir("/srv/shared")
	e.cfg.Directories = []config.Directory{{Path: "/srv/shared"}}
	e.reconcile()
	cfgID := e.entry("/srv/shared").ProjectID
	e.volume("a", nil, map[string]string{"type": "none", "o": "bind", "device": "/srv/shared"})
	e.volume("b", nil, map[string]string{"type": "none", "o": "bind", "device": "/srv/shared"})
	e.reconcile()
	got := e.entry("/srv/shared")
	if got == nil || got.Source != "volume" || got.Volume != "a" || got.ProjectID != cfgID {
		t.Fatalf("winner: %+v", got)
	}
	// Both losers share the path, so one series says duplicate_path (ADM-1).
	if len(e.c.view.unmeas) != 1 || e.c.view.unmeas[0] != (unmeasurable{"/srv/shared", reasonDuplicatePath}) {
		t.Fatalf("losers: %+v", e.c.view.unmeas)
	}
}

func TestChangedTopIDTriggersWalk(t *testing.T) {
	e := newEnv(t)
	e.volume("v", nil, nil)
	e.reconcile()
	e.walkAll()
	e.fs.projid[volPathOf("v")] = 0
	e.reconcile()
	if !e.entry(volPathOf("v")).Assigning {
		t.Fatal("re-walk expected")
	}
	e.walkAll()
	e.fs.projid[volPathOf("v")] = 7
	e.reconcile()
	if e.reason(volPathOf("v")) != reasonForeignID {
		t.Fatal("foreign expected")
	}
}

func TestWalkFailureIsRetriedLater(t *testing.T) {
	e := newEnv(t)
	e.volume("v", nil, nil)
	e.reconcile()
	e.fs.walkErr = errors.New("eperm")
	e.walkAll()
	if len(e.fs.walks) != 1 {
		t.Fatalf("one attempt expected: %d", len(e.fs.walks))
	}
	e.fs.walkErr = nil
	e.now = e.now.Add(retryFailedWalksAfter + time.Second)
	e.walkAll()
	if len(e.fs.walks) != 2 || e.entry(volPathOf("v")).Assigning {
		t.Fatal("retry expected")
	}
}

func TestRunWalkerAndImageLoop(t *testing.T) {
	e := newEnv(t)
	e.volume("v", nil, nil)
	e.reconcile()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.c.RunWalker(ctx); close(done) }()
	assigning := func() bool {
		e.c.mu.Lock()
		defer e.c.mu.Unlock()
		return e.entry(volPathOf("v")).Assigning
	}
	deadline := time.Now().Add(5 * time.Second)
	for assigning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if !e.c.st.Entries[state.VolumeKey("v")].WalkDone {
		t.Fatal("walker did not run")
	}
}

func TestSave(t *testing.T) {
	e := newEnv(t)
	if e.c.Config() == nil || e.c.DockerRoot() != "" {
		t.Fatal("config")
	}
	e.c.Save()
}

// A volume whose owner container is excluded is excluded and reset like
// exclude.volumes; a volume the excluded container only mounts stays.
func TestExcludedOwner(t *testing.T) {
	e := newEnv(t)
	e.volume("db", nil, nil)
	e.volume("shared", nil, nil)
	e.volume("tagged", nil, nil)
	e.volume("byimage", nil, nil)
	e.container("c1", "db1", "running", 1, nil, rw("db"), ro("shared"))
	e.container("c2", "app", "running", 2, nil, rw("shared"))
	e.container("c3", "x", "running", 3, map[string]string{config.LabelEnable: "false"}, rw("tagged"))
	e.container("c4", "cache", "running", 4, nil, rw("byimage"))
	e.container("c5", "gone", "running", 5, nil)
	e.dk.configs = map[string]string{"c1": "postgres:16", "c2": "app:1", "c3": "x", "c4": "redis:7"}
	e.reconcile()
	e.walkAll()
	if len(e.c.view.entries) != 3 || e.entry(volPathOf("tagged")) != nil {
		t.Fatalf("only the enable label excludes at first: %+v", e.c.view.entries)
	}
	e.cfg.Exclude = config.Exclude{Containers: []string{"db*"}, Images: []string{"docker.io/library/redis*"}}
	e.reconcile()
	// A Swarm task's image is matched without the digest Swarm pinned (REF-4).
	e.container("swarm", "swarm", "running", 1, map[string]string{ref.SwarmTaskLabel: "t1"})
	e.dk.configs["swarm"] = "redis:7@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	e.cfg.Exclude.Images = append(e.cfg.Exclude.Images, "redis:7")
	snap, err := e.c.readDocker(context.Background(), e.cfg)
	if err != nil || !snap.excluded["swarm"] {
		t.Fatalf("swarm task not excluded by tag: %v %v", snap.excluded, err)
	}
	delete(e.c.cfgImages, "swarm")
	e.dk.containers = e.dk.containers[:len(e.dk.containers)-1]
	for _, v := range []string{"db", "byimage"} {
		if e.entry(volPathOf(v)) != nil || !e.c.st.Entries[state.VolumeKey(v)].PendingReset {
			t.Fatalf("%s must be excluded and reset", v)
		}
	}
	if e.entry(volPathOf("shared")) == nil {
		t.Fatal("shared has another owner and stays")
	}
	n := e.dk.inspects
	e.reconcile()
	if e.dk.inspects != n+1 {
		t.Fatal("Config.Image is cached per container ID; only the 404 one is asked again")
	}
	e.dk.containers = e.dk.containers[:1]
	e.reconcile()
	if len(e.c.cfgImages) != 1 {
		t.Fatalf("cache not pruned: %v", e.c.cfgImages)
	}
	e.dk.inspectErr = errDown
	e.dk.containers = append(e.dk.containers, docker.Container{ID: "c9", Names: []string{"/new"}})
	e.c.Reconcile(context.Background())
	if e.c.recErrors == 0 {
		t.Fatal("inspect error must fail the reconcile")
	}
}

// Disabled directories keep their IDs; rules by stack and path reach them,
// their own labels win.
func TestDirectoriesDisabledAndRules(t *testing.T) {
	e := newEnv(t)
	e.fs.mkdir("/srv/x")
	e.cfg.Directories = []config.Directory{{Path: "/srv/x", Labels: map[string]string{"stack": "s", "team": "own"}}}
	e.cfg.Labels = []config.LabelRule{
		{Match: config.Match{Stack: "s"}, Labels: map[string]string{"team": "rule", "tier": "cold", "empty": ""}},
		{Match: config.Match{Path: "/srv/*"}, Labels: map[string]string{"site": "a"}},
		{Match: config.Match{Volume: "*"}, Labels: map[string]string{"vol": "only"}},
	}
	e.reconcile()
	e.walkAll()
	got := e.entry("/srv/x").Labels
	if got["team"] != "own" || got["tier"] != "cold" || got["site"] != "a" || got["vol"] != "" || got["empty"] != "" {
		t.Fatalf("labels: %v", got)
	}
	e.cfg.Disable = []string{config.FnDirectory}
	e.reconcile()
	if e.entry("/srv/x") != nil || e.c.st.Entries[state.ConfigKey("/srv/x")].PendingReset {
		t.Fatal("directory must be kept silently")
	}
}
