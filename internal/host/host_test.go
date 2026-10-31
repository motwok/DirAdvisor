// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"strings"
	"testing"
)

const mountinfo = `22 1 8:1 / / rw,relatime - overlay overlay rw,lowerdir=x
30 22 253:1 / /host rw,relatime master:1 - ext4 /dev/mapper/vg-root rw
31 30 253:2 / /host/var/lib/docker rw,relatime master:2 - ext4 /dev/mapper/vg-docker rw
32 30 253:3 / /host/srv/my\040data rw - xfs /dev/sdb1 rw,prjquota
33 30 0:50 / /host/tank rw - zfs tank/data rw,xattr
34 22 253:2 /containers/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef/hostname /etc/hostname rw - ext4 /dev/mapper/vg-docker rw
bad line
`

func TestParseMountinfo(t *testing.T) {
	ms := parseMountinfo(strings.NewReader(mountinfo))
	if len(ms) != 6 {
		t.Fatalf("got %d mounts", len(ms))
	}
	if ms[3].Point != "/host/srv/my data" || ms[3].FSType != "xfs" || !hasOption(ms[3].Options, "prjquota") {
		t.Fatalf("xfs mount: %+v", ms[3])
	}
	m, ok := longestMount(ms, "/host/var/lib/docker/volumes/v/_data")
	if !ok || m.Source != "/dev/mapper/vg-docker" {
		t.Fatalf("longest: %+v", m)
	}
	m, _ = longestMount(ms, "/host/srv/other")
	if m.Point != "/host" {
		t.Fatalf("fallback: %+v", m)
	}
	m, _ = longestMount(ms, "/data")
	if m.Point != "/" {
		t.Fatalf("root: %+v", m)
	}
	if _, ok := longestMount(nil, "/x"); ok {
		t.Fatal("no mounts")
	}
}

func TestToHost(t *testing.T) {
	cases := [][3]string{{"/host", "/host/srv/x", "/srv/x"}, {"/host", "/host", "/"}, {"/", "/srv/x", "/srv/x"}, {"/host", "/other", "/other"}}
	for _, c := range cases {
		if got := toHost(c[0], c[1]); got != c[2] {
			t.Errorf("toHost(%q,%q) = %q", c[0], c[1], got)
		}
	}
}

func TestContainerID(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got := ContainerIDFrom(mountinfo, ""); got != id {
		t.Fatalf("mountinfo: %q", got)
	}
	other := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	shm := "35 30 0:60 / /host/var/lib/docker/containers/" + other + "/mounts/shm rw - tmpfs shm rw\n"
	if got := ContainerIDFrom(shm+mountinfo, ""); got != id {
		t.Fatalf("propagated mount of another container won: %q", got)
	}
	if got := ContainerIDFrom(shm, ""); got != "" {
		t.Fatalf("no own bind: %q", got)
	}
	if got := ContainerIDFrom("", "0::/system.slice/docker-"+id+".scope\n"); got != id {
		t.Fatalf("cgroup v2: %q", got)
	}
	if got := ContainerIDFrom("", "12:memory:/docker/"+id+"\n"); got != id {
		t.Fatalf("cgroup v1: %q", got)
	}
	if ContainerIDFrom("", "0::/\n") != "" {
		t.Fatal("no id expected")
	}
}

func TestParseProjectspace(t *testing.T) {
	m := parseProjectspace("100000\t4096\t3\n100001\t0\t1\nbogus\nx\t1\t1\n")
	if len(m) != 2 || m[100000].Bytes != 4096 || m[100000].Inodes != 3 {
		t.Fatalf("projectspace: %+v", m)
	}
}

func TestMisc(t *testing.T) {
	if !zfsFeatureOK("active\n") || !zfsFeatureOK("enabled") || zfsFeatureOK("disabled") {
		t.Fatal("zfsFeatureOK")
	}
	if !AdminAction(UpgradeKernel) || AdminAction(RemoteFilesystem) || AdminAction("path_missing") {
		t.Fatal("AdminAction")
	}
	if hasOption("rw,relatime", "prjquota", "pquota") {
		t.Fatal("hasOption")
	}
	if unescape(`a\134b`) != `a\b` || unescape(`a\9`) != `a\9` {
		t.Fatal("unescape")
	}
}
