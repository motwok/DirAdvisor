// SPDX-License-Identifier: AGPL-3.0-or-later

package winhost

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// needVolume opens the volume of the temp dir; that needs administrator
// rights, which the CI runners have.
func needVolume(t *testing.T) (*volume, string) {
	t.Helper()
	base := t.TempDir()
	point, _, err := volumeOf(base)
	if err != nil {
		t.Fatal(err)
	}
	v, err := openVolume(point)
	if err != nil {
		if os.Getenv("CI") == "" {
			t.Skipf("needs administrator rights: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CloseHandle(v.h); windows.CloseHandle(v.hint) })
	return v, base
}

func write(t *testing.T, p string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// exact walks a tree opening every entry; files below skip count elsewhere.
func exact(t *testing.T, root string, skip ...string) Usage {
	t.Helper()
	var u Usage
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		for _, s := range skip {
			if p == s {
				return filepath.SkipDir
			}
		}
		u.Inodes++
		if !d.IsDir() {
			h, err := open(p, windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_BACKUP_SEMANTICS)
			if err != nil {
				t.Fatal(err)
			}
			alloc, _, _ := sizes(h)
			windows.CloseHandle(h)
			u.Bytes += uint64(alloc)
		}
		return nil
	})
	return u
}

func settle(t *testing.T, v *volume) {
	t.Helper()
	if err := v.poll(); err != nil {
		t.Fatal(err)
	}
}

func TestVolumeTracking(t *testing.T) {
	v, base := needVolume(t)
	a, inner, outside := filepath.Join(base, "a"), filepath.Join(base, "a", "inner"), filepath.Join(base, "outside")
	write(t, filepath.Join(a, "f1"), 100000)
	write(t, filepath.Join(a, "sub", "f2"), 100000)
	write(t, filepath.Join(inner, "f3"), 100000)
	write(t, filepath.Join(outside, "dir", "f4"), 100000)
	ctx := context.Background()
	if err := v.assign(ctx, a, 10); err != nil {
		t.Fatal(err)
	}
	if err := v.assign(ctx, inner, 11); err != nil {
		t.Fatal(err)
	}
	check := func(stage string) {
		t.Helper()
		settle(t, v)
		u := v.usage(1)
		if u[10] != exact(t, a, inner) || u[11] != exact(t, inner) {
			t.Fatalf("%s: tracked %+v, exact %+v / %+v", stage, u, exact(t, a, inner), exact(t, inner))
		}
	}
	check("assigned")
	if id, err := v.id(a); err != nil || id != 10 {
		t.Fatalf("id %d %v", id, err)
	}
	if id, _ := v.id(base); id != 0 {
		t.Fatal("base is not assigned")
	}
	if _, err := v.id(filepath.Join(base, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}

	write(t, filepath.Join(a, "new"), 200000)
	f, _ := os.OpenFile(filepath.Join(a, "f1"), os.O_APPEND|os.O_WRONLY, 0)
	f.Write(make([]byte, 300000))
	f.Close()
	os.Truncate(filepath.Join(a, "sub", "f2"), 0)
	os.Remove(filepath.Join(inner, "f3"))
	os.Rename(filepath.Join(a, "sub"), filepath.Join(a, "sub2"))
	os.Rename(filepath.Join(a, "new"), filepath.Join(outside, "new"))
	os.Rename(filepath.Join(outside, "dir"), filepath.Join(a, "dir"))
	check("changed")

	g, err := os.Create(filepath.Join(a, "grow"))
	if err != nil {
		t.Fatal(err)
	}
	g.Write(make([]byte, 1<<20))
	g.Sync()
	check("open 1 MB")
	g.Write(make([]byte, 4<<20))
	g.Sync()
	check("open 5 MB")
	g.Close()
	check("closed")

	if err := v.assign(ctx, inner, 0); err != nil {
		t.Fatal(err)
	}
	if u := v.usage(1); u[10] != exact(t, a) || u[11] != (Usage{}) {
		t.Fatalf("inner released: %+v", u)
	}
	v.rewalk()
	if u := v.usage(11); u[10] != (Usage{}) || len(u) != 0 {
		t.Fatalf("min: %+v", u)
	}
	if u := v.usage(1); u[10] != exact(t, a) {
		t.Fatalf("rewalk: %+v", u)
	}
}

func TestVolumeEdges(t *testing.T) {
	v, base := needVolume(t)
	ctx := context.Background()
	if err := v.assign(ctx, filepath.Join(base, "missing"), 1); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	a := filepath.Join(base, "a")
	write(t, filepath.Join(a, "sub", "f"), 10)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := v.assign(canceled, a, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	calls := 0
	dirInfo = func(h windows.Handle, class uint32, buf *byte, n uint32) error {
		if calls++; calls > 2 {
			return errors.New("denied")
		}
		return windows.GetFileInformationByHandleEx(h, class, buf, n)
	}
	err := v.assign(ctx, a, 1)
	dirInfo = windows.GetFileInformationByHandleEx
	if err != nil {
		t.Fatalf("an unreadable subdirectory counts as empty: %v", err)
	}
	dirInfo = func(windows.Handle, uint32, *byte, uint32) error { return errors.New("denied") }
	err = v.assign(ctx, filepath.Join(base), 2)
	dirInfo = windows.GetFileInformationByHandleEx
	if err == nil {
		t.Fatal("an unreadable root is an error")
	}

	v.nodes[1], v.nodes[2] = &node{parent: 2}, &node{parent: 1}
	v.usage(1)
	delete(v.nodes, 1)
	delete(v.nodes, 2)

	openByID = func(windows.Handle, uint64) (windows.Handle, error) { return windows.InvalidHandle, errors.New("gone") }
	if _, ok := v.measure(1); ok {
		t.Fatal("measure")
	}
	if _, err := v.pathOf(1); err == nil {
		t.Fatal("pathOf")
	}
	os.Rename(filepath.Join(base, "x"), filepath.Join(a, "x"))
	os.MkdirAll(filepath.Join(base, "moved"), 0o755)
	os.Rename(filepath.Join(base, "moved"), filepath.Join(a, "moved"))
	settle(t, v)
	openByID = openFileByID
	fid, _ := fileID(filepath.Join(a, "sub", "f"))
	dirInfo = func(windows.Handle, uint32, *byte, uint32) error { return errors.New("denied") }
	if _, ok := v.measure(fid); ok {
		t.Fatal("measure without sizes")
	}
	dirInfo = windows.GetFileInformationByHandleEx
	finalPathName = func(windows.Handle, *uint16, uint32, uint32) (uint32, error) { return 0, errors.New("no name") }
	if _, err := v.pathOf(fid); err == nil {
		t.Fatal("pathOf without name")
	}
	finalPathName = windows.GetFinalPathNameByHandle

	ioctl = func(h windows.Handle, code uint32, in *byte, inLen uint32, out *byte, outLen uint32, n *uint32, o *windows.Overlapped) error {
		if code == fsctlReadUsnJournal {
			return errors.New("journal wrapped")
		}
		return windows.DeviceIoControl(h, code, in, inLen, out, outLen, n, o)
	}
	if err := v.poll(); err != nil {
		t.Fatalf("a lost journal means a rewalk: %v", err)
	}
	ioctl = func(windows.Handle, uint32, *byte, uint32, *byte, uint32, *uint32, *windows.Overlapped) error {
		return errors.New("no journal")
	}
	if err := v.poll(); err == nil {
		t.Fatal("no journal at all")
	}
	ioctl = windows.DeviceIoControl
}

// A root assigned during a rewalk stays; a root whose directory is gone is
// dropped by the rewalk and by the journal.
func TestRewalkRootsChange(t *testing.T) {
	v, base := needVolume(t)
	ctx := context.Background()
	a, b, gone := filepath.Join(base, "a"), filepath.Join(base, "b"), filepath.Join(base, "gone")
	for _, d := range []string{a, b, gone} {
		write(t, filepath.Join(d, "f"), 1000)
	}
	if err := v.assign(ctx, a, 10); err != nil {
		t.Fatal(err)
	}
	if err := v.assign(ctx, gone, 12); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(gone)
	saved := rewalkHook
	defer func() { rewalkHook = saved }()
	rewalkHook = func() {
		rewalkHook = func() {}
		if err := v.assign(ctx, b, 11); err != nil {
			t.Error(err)
		}
	}
	v.rewalk()
	u := v.usage(1)
	if u[10] != exact(t, a) || u[11] != exact(t, b) || len(v.roots) != 2 || u[12] != (Usage{}) {
		t.Fatalf("usage %+v roots %v", u, v.roots)
	}
	os.RemoveAll(b)
	settle(t, v)
	if len(v.roots) != 1 || v.usage(1)[11] != (Usage{}) {
		t.Fatalf("roots %v", v.roots)
	}
}

func TestOpenVolumeErrors(t *testing.T) {
	v, base := needVolume(t)
	if _, err := openVolume(`Q:\`); err == nil {
		t.Skip("drive Q: exists")
	}
	ioctl = func(windows.Handle, uint32, *byte, uint32, *byte, uint32, *uint32, *windows.Overlapped) error {
		return errors.New("no journal")
	}
	_, err := openVolume(v.point)
	ioctl = windows.DeviceIoControl
	if err == nil {
		t.Fatal("no journal")
	}
	volMountName = func(*uint16, *uint16, uint32) error { return errors.New("x") }
	_, err = openVolume(v.point)
	volMountName = windows.GetVolumeNameForVolumeMountPoint
	if err == nil {
		t.Fatal("no volume name")
	}
	calls := 0
	createFile = func(name *uint16, access, mode uint32, sa *windows.SecurityAttributes, disp, attrs uint32, tmpl windows.Handle) (windows.Handle, error) {
		if calls++; calls > 1 {
			return windows.InvalidHandle, errors.New("denied")
		}
		return windows.CreateFile(name, access, mode, sa, disp, attrs, tmpl)
	}
	_, err = openVolume(v.point)
	createFile = windows.CreateFile
	if err == nil {
		t.Fatal("no hint handle")
	}
	_ = base
}

func TestNTFSHelpers(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "f"), 10)
	if utf16le([]byte{'a', 0, 'b', 0}) != "ab" || volKey(`C:\`) != "C:" || volKey("c:") != "C:" {
		t.Fatal("helpers")
	}
	for in, want := range map[string]string{`C:\x\`: `\\?\C:\x`, `C:\`: `\\?\C:\`, `\\?\C:\x`: `\\?\C:\x`,
		`\\.\C:`: `\\.\C:`, `\\srv\share\x`: `\\?\UNC\srv\share\x`} {
		if got := extended(in); got != want {
			t.Errorf("extended(%q) = %q", in, got)
		}
	}
	if _, err := volumeName("a\x00b"); err == nil {
		t.Fatal("NUL volume")
	}
	point, _, _ := volumeOf(base)
	if dev, err := volumeName(point); err != nil || !strings.HasPrefix(dev, `\\?\Volume{`) || strings.HasSuffix(dev, `\`) {
		t.Fatalf("volume name %q %v", dev, err)
	}
	volMountName = func(*uint16, *uint16, uint32) error { return errors.New("x") }
	_, err := volumeName(point)
	volMountName = windows.GetVolumeNameForVolumeMountPoint
	if err == nil {
		t.Fatal("volume name error")
	}
	// A name Win32 would normalize away is opened as it is.
	dot := filepath.Join(base, "dot.")
	if err := windows.CreateDirectory(windows.StringToUTF16Ptr(extended(dot)), nil); err != nil {
		t.Fatal(err)
	}
	write(t, extended(filepath.Join(dot, "f")), 10)
	if es, err := list(dot); err != nil || len(es) != 1 {
		t.Fatalf("list dot %v %v", es, err)
	}
	nodes := map[uint64]*node{}
	if err := walk(base, 1, nodes); err != nil || len(nodes) != 4 {
		t.Fatalf("walk dot %d %v", len(nodes), err)
	}
	if err := walk(filepath.Join(base, "missing"), 2, nodes); err == nil || nodes[2] != nil {
		t.Fatal("walk missing")
	}
	if _, err := fileID("a\x00b"); err == nil {
		t.Fatal("NUL in path")
	}
	if _, err := fileID(filepath.Join(base, "missing")); err == nil {
		t.Fatal("missing")
	}
	handleInfo = func(windows.Handle, *windows.ByHandleFileInformation) error { return errors.New("x") }
	_, err = fileID(base)
	handleInfo = windows.GetFileInformationByHandle
	if err == nil {
		t.Fatal("handle info")
	}
	if es, err := list(base); err != nil || len(es) != 2 || es[0].name != "dot." || es[1].name != "f" {
		t.Fatalf("list %v %v", es, err)
	}
	if _, err := list(filepath.Join(base, "missing")); err == nil {
		t.Fatal("list missing")
	}
	if _, _, err := volumeOf("a\x00b"); err == nil {
		t.Fatal("NUL")
	}
	volPathName = func(*uint16, *uint16, uint32) error { return errors.New("x") }
	_, _, err = volumeOf(base)
	volPathName = windows.GetVolumePathName
	if err == nil {
		t.Fatal("volume path")
	}
	volInfo = func(*uint16, *uint16, uint32, *uint32, *uint32, *uint32, *uint16, uint32) error {
		return errors.New("x")
	}
	_, _, err = volumeOf(base)
	volInfo = windows.GetVolumeInformation
	if err == nil {
		t.Fatal("volume information")
	}
	if err := enableBackup(); err != nil && os.Getenv("CI") != "" {
		t.Fatal(err)
	}
	processToken = func(windows.Handle, uint32, *windows.Token) error { return errors.New("x") }
	if enableBackup() == nil {
		t.Fatal("token")
	}
	processToken = windows.OpenProcessToken
	privilege = func(*uint16, *uint16, *windows.LUID) error { return errors.New("x") }
	if enableBackup() == nil {
		t.Fatal("privilege")
	}
	privilege = windows.LookupPrivilegeValue
	if _, err := openFileByID(windows.InvalidHandle, 0); err == nil {
		t.Fatal("open by ID")
	}
	ioctl = func(windows.Handle, uint32, *byte, uint32, *byte, uint32, *uint32, *windows.Overlapped) error {
		return errors.New("x")
	}
	_, qerr := queryJournal(windows.InvalidHandle)
	_, _, rerr := readJournal(windows.InvalidHandle, 1, 1)
	ioctl = windows.DeviceIoControl
	if qerr == nil || rerr == nil {
		t.Fatal("journal errors")
	}
	if notExist(errors.New("x")) == nil || !errors.Is(notExist(windows.ERROR_PATH_NOT_FOUND), fs.ErrNotExist) {
		t.Fatal("notExist")
	}
	_ = time.Second
}
