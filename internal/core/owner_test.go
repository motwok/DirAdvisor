// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"strings"
	"testing"

	"github.com/motwok/DirAdvisor/internal/config"
	"github.com/motwok/DirAdvisor/internal/docker"
	"github.com/motwok/DirAdvisor/internal/host"
	"github.com/motwok/DirAdvisor/internal/state"
)

func ctr(id, name, st string, created int64, stack string, mounts ...docker.Mount) docker.Container {
	l := map[string]string{}
	if stack != "" {
		l[composeProject] = stack
		l[composeWorkingDir] = "/opt/" + stack
	}
	return docker.Container{ID: id, Names: []string{"/" + name}, State: st, Created: created, Labels: l, Mounts: mounts}
}

func TestOwnerComposeLabelWins(t *testing.T) {
	vol := docker.Volume{Name: "a_data", Labels: map[string]string{composeProject: "a"}}
	cs := []docker.Container{
		ctr("1", "b-x-1", "running", 1, "b", rw("a_data")),
		ctr("2", "a-db-1", "exited", 5, "a", rw("a_data")),
		ctr("3", "a-ro-1", "running", 0, "a", ro("a_data")),
	}
	o := resolveOwner(vol, cs, nil, "")
	if o.Stack != "a" || o.Owner != "a-db-1" || o.State != StateStopped || o.WorkingDir != "/opt/a" || !o.Used {
		t.Fatalf("%+v", o)
	}
	// No writer of stack a: remembered owner stays, stack stays a.
	o = resolveOwner(vol, cs[:1], &state.Entry{Owner: "a-db-1", Stack: "a"}, "")
	if o.Stack != "a" || o.Owner != "a-db-1" || o.State != StateStopped {
		t.Fatalf("remembered: %+v", o)
	}
	o = resolveOwner(vol, nil, nil, "")
	if o.Stack != "a" || o.Owner != "" || o.State != StateStopped {
		t.Fatalf("label only: %+v", o)
	}
}

func TestOwnerWithoutLabel(t *testing.T) {
	vol := docker.Volume{Name: "ext"}
	cs := []docker.Container{
		ctr("1", "s-new-1", "running", 20, "s", rw("ext")),
		ctr("2", "s-old-1", "running", 10, "s", rw("ext")),
	}
	o := resolveOwner(vol, cs, nil, "")
	if o.Owner != "s-old-1" || o.Stack != "s" || o.State != StateRunning {
		t.Fatalf("oldest: %+v", o)
	}
	o = resolveOwner(vol, cs, &state.Entry{Owner: "s-new-1", Stack: "s"}, "")
	if o.Owner != "s-new-1" {
		t.Fatalf("remembered first user: %+v", o)
	}
	mixed := append(cs, ctr("3", "plain", "running", 1, "", rw("ext")))
	o = resolveOwner(vol, mixed, nil, "")
	if o.Owner != "" || o.Stack != "" || o.State != StateUnused {
		t.Fatalf("different stacks: %+v", o)
	}
	o = resolveOwner(vol, []docker.Container{ctr("3", "plain", "exited", 1, "", rw("ext"))}, nil, "")
	if o.Owner != "plain" || o.Stack != "" || o.State != StateStopped {
		t.Fatalf("no stack: %+v", o)
	}
	o = resolveOwner(vol, nil, &state.Entry{Owner: "gone", Stack: "s"}, "")
	if o.Owner != "gone" || o.Stack != "s" || o.State != StateStopped {
		t.Fatalf("down: %+v", o)
	}
	o = resolveOwner(vol, nil, nil, "")
	if o.State != StateUnused {
		t.Fatalf("unused: %+v", o)
	}
}

func TestOwnerSelf(t *testing.T) {
	vol := docker.Volume{Name: "da_data", Labels: map[string]string{composeProject: "da"}}
	self := "abcdef1234567890"
	cs := []docker.Container{ctr(self+"ffff", "diradvisor", "running", 1, "da", rw("da_data"))}
	o := resolveOwner(vol, cs, nil, self[:12])
	if o.Owner != "" || o.State != StateRunning || o.Stack != "da" {
		t.Fatalf("self: %+v", o)
	}
}

func TestLabels(t *testing.T) {
	cfg := config.Default()
	cfg.Docker.ContainerLabels = []config.LabelRef{{From: "com.example.team"}, {From: "x", To: "renamed"}}
	cfg.Docker.VolumeLabels = []config.LabelRef{{From: "retention"}}
	cfg.Docker.Labels = []config.LabelRule{
		{Match: config.Match{Stack: "app"}, Labels: map[string]string{"team": "rule"}},
		{Match: config.Match{Volume: "nomatch"}, Labels: map[string]string{"never": "x"}},
		{Match: config.Match{Container: "app-*", Path: "/var/lib/docker/volumes/*/_data"}, Labels: map[string]string{"tier": "hot"}},
	}
	vol := docker.Volume{Name: "app_data", Labels: map[string]string{"retention": "30d", "diradvisor.label.team": "vol", "diradvisor.label.path": "bad"}}
	c := ctr("1", "app-web-1", "running", 1, "app")
	c.Labels["com.example.team"] = "ops"
	c.Labels["x"] = "y"
	c.Labels["diradvisor.label.team"] = "ctr"
	c.Labels["diradvisor.label.site-a"] = "home"
	// Invalid after sanitizing: dropped, never written (would break the scrape).
	c.Labels["diradvisor.label.1st"] = "x"
	c.Labels["diradvisor.label."] = "x"
	vol.Labels["diradvisor.label.__x"] = "x"
	o := ownership{Owner: "app-web-1", Stack: "app", OwnerC: &c}
	l, ctr := volumeLabels(cfg, vol, volPath, o, nil)
	want := map[string]string{
		"container_label_com_example_team": "ops", "renamed": "y", "volume_label_retention": "30d",
		"team": "rule", "site_a": "home", "tier": "hot",
	}
	if len(l) != len(want) {
		t.Fatalf("labels %+v", l)
	}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("%s = %q, want %q (all %+v)", k, l[k], v, l)
		}
	}
	if len(ctr) != 4 || ctr["container_label_com_example_team"] != "ops" || ctr["renamed"] != "y" || ctr["site_a"] != "home" || ctr["team"] != "ctr" {
		t.Fatalf("container labels: %+v", ctr)
	}
	// Without owner container the container-derived labels come from the state.
	l, ctr = volumeLabels(cfg, vol, volPath, ownership{Stack: "app"}, map[string]string{"container_label_com_example_team": "old", "renamed": "r", "site_a": "x"})
	if l["container_label_com_example_team"] != "old" || l["renamed"] != "r" || l["site_a"] != "x" || l["team"] != "rule" || len(ctr) != 3 {
		t.Fatalf("from state: %+v %+v", l, ctr)
	}
}

func TestImageCheck(t *testing.T) {
	e := newEnv(t)
	if e.c.img.mode() != "disabled" {
		t.Fatal("default disabled")
	}
	e.cfg.Global.ImageUpdateCheck.Enabled = true
	e.c.CheckImage(context.Background())
	if e.c.img.mode() != "unknown" {
		t.Fatalf("unknown expected, got %s", e.c.img.mode())
	}
	e.dk.self = docker.ContainerJSON{ID: "self0000000000", Image: "sha256:1"}
	e.dk.self.Config.Image = "ghcr.io/motwok/diradvisor:1.0.0"
	e.c.CheckImage(context.Background())
	if e.c.img.mode() != "pinned" {
		t.Fatal("pinned expected")
	}
	e.dk.self.Config.Image = "ghcr.io/motwok/diradvisor"
	e.dk.digests = []string{"ghcr.io/motwok/diradvisor@sha256:aa"}
	e.dk.remote = "sha256:aa"
	e.c.CheckImage(context.Background())
	if e.c.img.mode() != "latest" || e.c.img.available || !e.c.img.success {
		t.Fatalf("same: %+v", e.c.img)
	}
	e.dk.remote = "sha256:bb"
	e.c.CheckImage(context.Background())
	first := e.c.img.since
	e.now = e.now.Add(3600e9)
	e.c.CheckImage(context.Background())
	if !e.c.img.available || e.c.img.since != first || first == 0 {
		t.Fatalf("available: %+v", e.c.img)
	}
	out := e.metricsText()
	for _, s := range []string{`diradvisor_image_update_check{mode="latest"} 1`, "diradvisor_image_update_available 1",
		"diradvisor_image_check_success 1", "diradvisor_image_update_available_since_timestamp_seconds"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %s", s)
		}
	}
	e.dk.remoteErr = context.DeadlineExceeded
	e.c.CheckImage(context.Background())
	if e.c.img.success || !e.c.img.available {
		t.Fatalf("failure keeps values: %+v", e.c.img)
	}
}

func TestSplitRef(t *testing.T) {
	cases := map[string][3]string{
		"ghcr.io/motwok/diradvisor:latest": {"ghcr.io/motwok/diradvisor", "latest", ""},
		"ghcr.io/motwok/diradvisor":        {"ghcr.io/motwok/diradvisor", "latest", ""},
		"localhost:5000/x":                 {"localhost:5000/x", "latest", ""},
		"repo@sha256:aa":                   {"repo", "", "digest"},
	}
	for in, want := range cases {
		r, tag, d := splitRef(in)
		if r != want[0] || tag != want[1] || (d != (want[2] == "digest")) {
			t.Errorf("splitRef(%q) = %q %q %v", in, r, tag, d)
		}
	}
	if normalizeRepo("docker.io/library/alpine") != "alpine" {
		t.Fatal("normalize")
	}
}

func TestCommands(t *testing.T) {
	m := hostMount("/srv", "zfs", "tank/data")
	for reason, want := range map[string]string{
		"enable_zfs_project_quota":    "zpool set feature@project_quota=enabled tank",
		"mount_with_prjquota":         "prjquota",
		"xfs_v4_group_quota_conflict": "v5",
		"install_zfs_userland":        "zfsutils",
		"upgrade_kernel":              "5.14",
		"filesystem_not_supported":    "ext4, XFS or ZFS",
		"other":                       "",
	} {
		if got := command(reason, m, "/srv/x", false); !strings.Contains(got, want) {
			t.Errorf("%s: %q", reason, got)
		}
	}
}

func TestCommandSteps(t *testing.T) {
	ext := hostMount("/var/lib/docker", "ext4", "/dev/mapper/vg-docker")
	for _, c := range []struct {
		reason string
		m      host.Mount
		docker bool
		want   []string
	}{
		{"enable_project_quota_feature", ext, true, []string{"systemctl stop docker docker.socket containerd",
			"umount /var/lib/docker", "tune2fs -O project,quota /dev/mapper/vg-docker", "mount /var/lib/docker",
			"systemctl start containerd docker", "no fstab change"}},
		{"enable_project_quota_feature", hostMount("/srv", "ext4", "/dev/sdb1"), false, []string{"stop everything using /srv", "start it again"}},
		{"enable_project_quota_feature", hostMount("/", "ext4", "/dev/sda1"), true, []string{"rescue system", "tune2fs -O project,quota /dev/sda1"}},
		{"mount_with_prjquota", hostMount("/srv", "xfs", "/dev/sdc1"), true, []string{"/etc/fstab", "/dev/sdc1",
			"systemctl stop docker", "umount /srv && mount /srv", "remount is not enough", "start Docker again"}},
		{"mount_with_prjquota", hostMount("/", "xfs", "/dev/sda1"), false, []string{"rootflags=prjquota", "reboot"}},
	} {
		got := command(c.reason, c.m, "/x", c.docker)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s %s: %q lacks %q", c.reason, c.m.Point, got, w)
			}
		}
		if !c.docker && strings.Contains(got, "Docker") {
			t.Errorf("%s %s: Docker not expected: %q", c.reason, c.m.Point, got)
		}
	}
}
