// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func fdPath(fd int) string {
	p, _ := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	return p
}

// fakeXattr replaces ioctl with an in-memory fsxattr store keyed by path.
type fakeXattr struct {
	attrs map[string]fsxattr
	fail  map[string]error
}

func useFakeXattr(t *testing.T) *fakeXattr {
	f := &fakeXattr{attrs: map[string]fsxattr{}, fail: map[string]error{}}
	old := ioctl
	ioctl = func(fd int, req uintptr, arg unsafe.Pointer) error {
		p := fdPath(fd)
		if err := f.fail[p]; err != nil {
			return err
		}
		x := (*fsxattr)(arg)
		if req == fsIOCGetXattr {
			*x = f.attrs[p]
		} else {
			f.attrs[p] = *x
		}
		return nil
	}
	t.Cleanup(func() { ioctl = old })
	return f
}

func useQuotactl(t *testing.T, fn func(id uint32, d *nextdqblk) error) {
	old := quotactlNext
	quotactlNext = func(_ int, id uint32, d *nextdqblk) error { return fn(id, d) }
	t.Cleanup(func() { quotactlNext = old })
}

func useRunTool(t *testing.T, fn func(bin string, args []string) (string, error)) {
	old := runTool
	runTool = func(_ *Linux, _ context.Context, bin string, args ...string) (string, error) { return fn(bin, args) }
	t.Cleanup(func() { runTool = old })
}

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxStatListDirs(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "a/f"))
	if err := os.Mkdir(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := New(root).(*Linux)
	if ok, dir := l.Stat("/a"); !ok || !dir {
		t.Fatal("stat dir")
	}
	if ok, _ := l.Stat("/missing"); ok {
		t.Fatal("stat missing")
	}
	ds, err := l.ListDirs("/")
	if err != nil || len(ds) != 2 {
		t.Fatalf("list: %v %v", ds, err)
	}
	if _, err := l.ListDirs("/missing"); err == nil {
		t.Fatal("list missing")
	}
	if New("/").(*Linux).join("/x/../y") != "/y" {
		t.Fatal("join")
	}
}

func TestLinuxMountOf(t *testing.T) {
	l := &Linux{Root: "/host"}
	old := mountinfoPath
	t.Cleanup(func() { mountinfoPath = old })
	mi := filepath.Join(t.TempDir(), "mountinfo")
	mountinfoPath = mi
	if _, err := l.MountOf("/srv"); err == nil {
		t.Fatal("missing mountinfo")
	}
	touch(t, mi)
	if _, err := l.MountOf("/srv"); err == nil {
		t.Fatal("no mounts")
	}
	if err := os.WriteFile(mi, []byte(mountinfo), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := l.MountOf("/srv/my data/x")
	if err != nil || m.Point != "/srv/my data" || m.FSType != "xfs" {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestLinuxProjectIDReal(t *testing.T) {
	l := New("/").(*Linux)
	if _, err := l.ProjectID(t.TempDir()); err != nil {
		t.Skipf("FS_IOC_FSGETXATTR not supported here: %v", err)
	}
	if _, err := l.ProjectID("/proc"); err == nil {
		t.Fatal("procfs has no fsxattr")
	}
	f := filepath.Join(t.TempDir(), "f")
	touch(t, f)
	if _, err := l.ProjectID(f); err == nil {
		t.Fatal("not a directory")
	}
	if err := sysQuotactlNext(-1, 0, &nextdqblk{}); err == nil {
		t.Fatal("bad fd")
	}
}

func TestLinuxProjectIDFake(t *testing.T) {
	fx := useFakeXattr(t)
	root := t.TempDir()
	fx.attrs[root] = fsxattr{Projid: 100007}
	l := New("/").(*Linux)
	if id, err := l.ProjectID(root); err != nil || id != 100007 {
		t.Fatalf("%d %v", id, err)
	}
	fx.fail[root] = syscall.ENOTTY
	if _, err := l.ProjectID(root); err == nil {
		t.Fatal("ioctl error")
	}
}

func TestLinuxQuotaStatus(t *testing.T) {
	root := t.TempDir()
	l := New(root).(*Linux)
	var qerr error
	useQuotactl(t, func(uint32, *nextdqblk) error { return qerr })
	cases := []struct {
		fs   string
		err  error
		want string
	}{
		{"ext4", nil, ""},
		{"xfs", syscall.ENOENT, ""},
		{"ext4", syscall.ENOSYS, UpgradeKernel},
		{"ext4", syscall.ESRCH, EnableProjectQuotaFeature},
		{"xfs", syscall.ESRCH, MountWithPrjquota},
		{"nfs4", nil, RemoteFilesystem},
		{"btrfs", nil, FilesystemNotSupported},
	}
	for _, c := range cases {
		qerr = c.err
		if got, err := l.QuotaStatus(Mount{Point: "/", FSType: c.fs}); err != nil || got != c.want {
			t.Errorf("%s %v: %q %v", c.fs, c.err, got, err)
		}
	}
	qerr = syscall.EIO
	if _, err := l.QuotaStatus(Mount{Point: "/", FSType: "ext4"}); !errors.Is(err, syscall.EIO) {
		t.Fatalf("EIO: %v", err)
	}
	if _, err := l.QuotaStatus(Mount{Point: "/missing", FSType: "xfs"}); err == nil {
		t.Fatal("missing mount point")
	}
}

func TestLinuxQuotaStatusZFS(t *testing.T) {
	root := t.TempDir()
	l := New(root).(*Linux)
	m := Mount{Point: "/tank", FSType: "zfs", Source: "tank/data"}
	if got, _ := l.QuotaStatus(m); got != InstallZFSUserland {
		t.Fatalf("no userland: %q", got)
	}
	touch(t, filepath.Join(root, "usr/sbin/zpool"))
	if got, _ := l.QuotaStatus(m); got != InstallZFSUserland {
		t.Fatalf("zfs missing: %q", got)
	}
	touch(t, filepath.Join(root, "sbin/zfs"))
	var out string
	var rerr error
	useRunTool(t, func(bin string, args []string) (string, error) {
		if bin != "/usr/sbin/zpool" || args[len(args)-1] != "tank" {
			t.Errorf("run %s %v", bin, args)
		}
		return out, rerr
	})
	out = "active\n"
	if got, err := l.QuotaStatus(m); got != "" || err != nil {
		t.Fatalf("active: %q %v", got, err)
	}
	out = "disabled\n"
	if got, _ := l.QuotaStatus(m); got != EnableZFSProjectQuota {
		t.Fatalf("disabled: %q", got)
	}
	rerr = errors.New("boom")
	if _, err := l.QuotaStatus(m); err == nil {
		t.Fatal("run error")
	}
}

func TestLinuxUsage(t *testing.T) {
	root := t.TempDir()
	l := New(root).(*Linux)
	m := Mount{Point: "/", FSType: "ext4"}
	ids := []uint32{100000, 100005}
	useQuotactl(t, func(id uint32, d *nextdqblk) error {
		for _, x := range ids {
			if x >= id {
				*d = nextdqblk{ID: x, Curspace: uint64(x), Curinodes: 1}
				return nil
			}
		}
		return syscall.ENOENT
	})
	u, err := l.Usage(context.Background(), m, 100000)
	if err != nil || len(u) != 2 || u[100005].Bytes != 100005 {
		t.Fatalf("%v %v", u, err)
	}
	ids = []uint32{^uint32(0)}
	if u, err := l.Usage(context.Background(), m, 0); err != nil || len(u) != 1 {
		t.Fatalf("max id: %v %v", u, err)
	}
	useQuotactl(t, func(uint32, *nextdqblk) error { return syscall.EIO })
	if _, err := l.Usage(context.Background(), m, 0); err == nil {
		t.Fatal("quotactl error")
	}
	if _, err := l.Usage(context.Background(), Mount{Point: "/missing", FSType: "xfs"}, 0); err == nil {
		t.Fatal("missing mount point")
	}
}

func TestLinuxUsageZFS(t *testing.T) {
	root := t.TempDir()
	l := New(root).(*Linux)
	m := Mount{Point: "/tank", FSType: "zfs", Source: "tank/data"}
	if _, err := l.Usage(context.Background(), m, 0); err == nil {
		t.Fatal("zfs not found")
	}
	touch(t, filepath.Join(root, "usr/bin/zfs"))
	var rerr error
	useRunTool(t, func(bin string, args []string) (string, error) {
		return "100000\t4096\t3\n", rerr
	})
	if u, err := l.Usage(context.Background(), m, 0); err != nil || u[100000].Bytes != 4096 {
		t.Fatalf("%v %v", u, err)
	}
	rerr = errors.New("boom")
	if _, err := l.Usage(context.Background(), m, 0); err == nil {
		t.Fatal("run error")
	}
}

func TestLinuxZFSVersions(t *testing.T) {
	root := t.TempDir()
	l := New(root).(*Linux)
	if mod, user := l.ZFSVersions(); user != "" {
		t.Fatalf("no zfs: %q %q", mod, user)
	}
	touch(t, filepath.Join(root, "sys/module/zfs/version"))
	if err := os.WriteFile(filepath.Join(root, "sys/module/zfs/version"), []byte("2.2.6-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(root, "usr/sbin/zfs"))
	useRunTool(t, func(bin string, args []string) (string, error) {
		return "zfs-2.2.6-1\nzfs-kmod-2.2.6-1\n", nil
	})
	if mod, user := l.ZFSVersions(); mod != "2.2.6-1" || user != "zfs-2.2.6-1" {
		t.Fatalf("%q %q", mod, user)
	}
}

func TestLinuxRun(t *testing.T) {
	l := New("/").(*Linux)
	if out, err := l.run(context.Background(), "echo", "hi"); err != nil || out != "hi\n" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := l.run(context.Background(), "false"); err == nil {
		t.Fatal("false")
	}
	// An empty chroot has no binary, so this fails as root and as user.
	if _, err := New(t.TempDir()).(*Linux).run(context.Background(), "/bin/true"); err == nil {
		t.Fatal("chroot")
	}
}

func TestLinuxWalk(t *testing.T) {
	fx := useFakeXattr(t)
	root := t.TempDir()
	touch(t, filepath.Join(root, "d/f"))
	touch(t, filepath.Join(root, "d/e/g"))
	touch(t, filepath.Join(root, "skip/h"))
	touch(t, filepath.Join(root, "gone"))
	if err := os.Symlink("d", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	fx.attrs[filepath.Join(root, "d")] = fsxattr{Projid: 5, Xflags: fsXflagProjInherit | 1}
	fx.fail[filepath.Join(root, "gone")] = syscall.ENOENT

	l := New("/").(*Linux)
	skip := map[string]bool{filepath.Join(root, "skip"): true}
	if err := l.Walk(context.Background(), root, 100001, true, skip, 1000); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "d", "d/f", "d/e", "d/e/g"} {
		if x := fx.attrs[filepath.Join(root, p)]; x.Projid != 100001 {
			t.Errorf("%s: %+v", p, x)
		}
	}
	if x := fx.attrs[filepath.Join(root, "d")]; x.Xflags != fsXflagProjInherit|1 {
		t.Errorf("inherit: %+v", x)
	}
	if _, ok := fx.attrs[filepath.Join(root, "skip/h")]; ok {
		t.Error("skipped dir entered")
	}
	if x := fx.attrs[filepath.Join(root, "d/f")]; x.Xflags != 0 {
		t.Errorf("file flags: %+v", x)
	}

	if err := l.Walk(context.Background(), root, 0, false, nil, 0); err != nil {
		t.Fatal(err)
	}
	if x := fx.attrs[filepath.Join(root, "d")]; x.Xflags != 1 || x.Projid != 0 {
		t.Errorf("reset: %+v", x)
	}
}

func TestLinuxWalkErrors(t *testing.T) {
	fx := useFakeXattr(t)
	root := t.TempDir()
	touch(t, filepath.Join(root, "d/f"))
	l := New("/").(*Linux)
	ctx := context.Background()

	if err := l.Walk(ctx, filepath.Join(root, "missing"), 1, true, nil, 0); err == nil {
		t.Fatal("missing root")
	}
	fx.fail[root] = syscall.EPERM
	if err := l.Walk(ctx, root, 1, true, nil, 0); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("root set: %v", err)
	}
	delete(fx.fail, root)
	fx.fail[filepath.Join(root, "d")] = syscall.EPERM
	if err := l.Walk(ctx, root, 1, true, nil, 0); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("dir set: %v", err)
	}
	delete(fx.fail, filepath.Join(root, "d"))
	fx.fail[filepath.Join(root, "d/f")] = syscall.EPERM
	if err := l.Walk(ctx, root, 1, true, nil, 0); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("file set: %v", err)
	}
	delete(fx.fail, filepath.Join(root, "d/f"))

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := l.Walk(cctx, root, 1, true, nil, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}

	w := walker{ctx: ctx}
	if err := w.dir(filepath.Join(root, "missing"), "/missing"); err != nil {
		t.Fatalf("vanished dir: %v", err)
	}
	if err := w.dir(filepath.Join(root, "d/f"), "/d/f"); err == nil {
		t.Fatal("not a directory")
	}
	if err := w.set(filepath.Join(root, "missing"), false); !vanished(err) {
		t.Fatalf("vanished file: %v", err)
	}
	if err := (&walker{ctx: ctx, tick: time.NewTicker(time.Millisecond)}).wait(); err != nil {
		t.Fatal(err)
	}
}

func TestSelfContainerID(t *testing.T) {
	if SelfContainerID() == "" {
		t.Fatal("empty")
	}
}

// /dev/shm is a separate tmpfs below /dev on usual hosts.
func TestLinuxWalkStaysOnFilesystem(t *testing.T) {
	var dev, shm syscall.Stat_t
	if syscall.Lstat("/dev", &dev) != nil || syscall.Lstat("/dev/shm", &shm) != nil || dev.Dev == shm.Dev {
		t.Skip("no separate /dev/shm")
	}
	fx := useFakeXattr(t)
	if err := New("/").Walk(context.Background(), "/dev", 7, true, nil, 0); err != nil {
		t.Skipf("/dev not walkable here: %v", err)
	}
	if _, ok := fx.attrs["/dev/shm"]; ok {
		t.Fatal("other filesystem entered")
	}
}
