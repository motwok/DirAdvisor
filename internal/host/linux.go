// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	fsIOCGetXattr      = 0x801c581f // _IOR('X', 31, struct fsxattr)
	fsIOCSetXattr      = 0x401c5820 // _IOW('X', 32, struct fsxattr)
	fsXflagProjInherit = 0x00000200
	sysQuotactlFd      = 443 // same number on amd64 and arm64
	qGetNextQuota      = 0x800009
	prjQuota           = 2
	ioprioClassIdle    = 3
	ioprioWhoProcess   = 1
)

type fsxattr struct {
	Xflags     uint32
	Extsize    uint32
	Nextents   uint32
	Projid     uint32
	Cowextsize uint32
	Pad        [8]byte
}

type nextdqblk struct {
	Bhardlimit uint64
	Bsoftlimit uint64
	Curspace   uint64
	Ihardlimit uint64
	Isoftlimit uint64
	Curinodes  uint64
	Btime      uint64
	Itime      uint64
	Valid      uint32
	ID         uint32
}

type Linux struct {
	Root string
}

// New returns the host access for --path.rootfs.
func New(root string) FS {
	return &Linux{Root: filepath.Clean(root)}
}

func (l *Linux) join(p string) string {
	if l.Root == "/" {
		return filepath.Clean(p)
	}
	return filepath.Join(l.Root, p)
}

func (l *Linux) Stat(p string) (bool, bool) {
	fi, err := os.Lstat(l.join(p))
	if err != nil {
		return false, false
	}
	return true, fi.IsDir()
}

func (l *Linux) ListDirs(p string) ([]string, error) {
	es, err := os.ReadDir(l.join(p))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		if e.Type().IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Replaced in tests.
var (
	mountinfoPath = "/proc/self/mountinfo"
	ioctl         = sysIoctl
	quotactlNext  = sysQuotactlNext
	runTool       = (*Linux).run
	kernelRelease = sysKernelRelease
)

func sysKernelRelease() string {
	var u syscall.Utsname
	if syscall.Uname(&u) != nil {
		return ""
	}
	b := make([]byte, 0, len(u.Release))
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// kernelBefore reports whether the running kernel is older than major.minor.
// An unparsable release counts as new.
func kernelBefore(major, minor int) bool {
	var ma, mi int
	if n, _ := fmt.Sscanf(kernelRelease(), "%d.%d", &ma, &mi); n < 2 {
		return false
	}
	return ma < major || ma == major && mi < minor
}

func (l *Linux) MountOf(p string) (Mount, error) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return Mount{}, err
	}
	defer f.Close()
	m, ok := longestMount(parseMountinfo(f), l.join(p))
	if !ok {
		return Mount{}, fmt.Errorf("no mount found for %s", p)
	}
	return Mount{Point: toHost(l.Root, m.Point), FSType: m.FSType, Source: m.Source, Options: m.Options}, nil
}

func sysIoctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

func openNoFollow(p string, dir bool) (int, error) {
	flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC | syscall.O_NONBLOCK
	if dir {
		flags |= syscall.O_DIRECTORY
	}
	return syscall.Open(p, flags, 0)
}

func (l *Linux) ProjectID(p string) (uint32, error) {
	fd, err := openNoFollow(l.join(p), true)
	if err != nil {
		return 0, err
	}
	defer syscall.Close(fd)
	var x fsxattr
	if err := ioctl(fd, fsIOCGetXattr, unsafe.Pointer(&x)); err != nil {
		return 0, err
	}
	return x.Projid, nil
}

func sysQuotactlNext(fd int, id uint32, d *nextdqblk) error {
	cmd := uintptr(qGetNextQuota<<8 | prjQuota)
	if _, _, e := syscall.Syscall6(sysQuotactlFd, uintptr(fd), cmd, uintptr(id), uintptr(unsafe.Pointer(d)), 0, 0); e != 0 {
		return e
	}
	return nil
}

func (l *Linux) QuotaStatus(m Mount) (string, error) {
	switch m.FSType {
	case "ext4", "xfs":
		fd, err := openNoFollow(l.join(m.Point), true)
		if err != nil {
			return "", err
		}
		defer syscall.Close(fd)
		var d nextdqblk
		err = quotactlNext(fd, 0, &d)
		switch {
		case err == nil || errors.Is(err, syscall.ENOENT):
			return "", nil
		case errors.Is(err, syscall.ENOSYS) && kernelBefore(5, 14):
			return UpgradeKernel, nil
		case errors.Is(err, syscall.ENOSYS) && m.FSType == "xfs":
			// XFS answers ENOSYS when no quota type is accounted at all.
			return MountWithPrjquota, nil
		case errors.Is(err, syscall.ESRCH):
			if m.FSType == "ext4" {
				return EnableProjectQuotaFeature, nil
			}
			return MountWithPrjquota, nil
		}
		return "", fmt.Errorf("quotactl_fd on %s: %w", m.Point, err)
	case "zfs":
		bin := l.zfsBinary("zpool")
		if bin == "" || l.zfsBinary("zfs") == "" {
			return InstallZFSUserland, nil
		}
		pool, _, _ := strings.Cut(m.Source, "/")
		ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
		defer cancel()
		out, err := runTool(l, ctx, bin, "get", "-Hp", "-o", "value", "feature@project_quota", pool)
		if err != nil {
			return "", err
		}
		if !zfsFeatureOK(out) {
			return EnableZFSProjectQuota, nil
		}
		return "", nil
	case "nfs", "nfs4", "cifs", "smb3", "fuse.sshfs":
		return RemoteFilesystem, nil
	}
	return FilesystemNotSupported, nil
}

func (l *Linux) Usage(ctx context.Context, m Mount, minID uint32) (map[uint32]Usage, error) {
	if m.FSType == "zfs" {
		bin := l.zfsBinary("zfs")
		if bin == "" {
			return nil, errors.New("zfs not found")
		}
		ctx, cancel := context.WithTimeout(ctx, toolTimeout)
		defer cancel()
		out, err := runTool(l, ctx, bin, "projectspace", "-Hp", "-o", "name,used,objused", m.Source)
		if err != nil {
			return nil, err
		}
		return parseProjectspace(out), nil
	}
	fd, err := openNoFollow(l.join(m.Point), true)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	res := map[uint32]Usage{}
	for id := minID; ; {
		var d nextdqblk
		err := quotactlNext(fd, id, &d)
		if errors.Is(err, syscall.ENOENT) {
			return res, nil
		}
		if err != nil {
			return nil, err
		}
		res[d.ID] = Usage{Bytes: d.Curspace, Inodes: d.Curinodes}
		if d.ID == ^uint32(0) {
			return res, nil
		}
		id = d.ID + 1
	}
}

var zfsDirs = []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin", "/usr/local/sbin"}

// toolTimeout bounds every zfs/zpool call (spec MEA-2); a suspended pool
// blocks them forever otherwise.
const toolTimeout = 10 * time.Second

// zfsBinary returns the path of a ZFS tool as seen from inside the chroot.
func (l *Linux) zfsBinary(name string) string {
	for _, d := range zfsDirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(l.join(p)); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

func (l *Linux) run(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if l.Root != "/" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: l.Root}
		cmd.Dir = "/"
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %v: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (l *Linux) ZFSVersions() (string, string) {
	mod, _ := os.ReadFile(l.join("/sys/module/zfs/version"))
	if len(mod) == 0 {
		mod, _ = os.ReadFile("/sys/module/zfs/version")
	}
	var user string
	if bin := l.zfsBinary("zfs"); bin != "" {
		ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
		defer cancel()
		user, _ = runTool(l, ctx, bin, "version")
	}
	return strings.TrimSpace(string(mod)), strings.TrimSpace(strings.Split(user, "\n")[0])
}

// Walk opens every directory and file relative to its parent (openat with
// O_NOFOLLOW), so an entry swapped for a symlink during the walk is never
// followed and the walk cannot leave the tree.
func (l *Linux) Walk(ctx context.Context, root string, id uint32, inherit bool, skip map[string]bool, perSecond int) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Best effort: only use otherwise idle disk time. The thread goes back to
	// the pool afterwards, so the old priority is restored.
	prio, _, _ := syscall.Syscall(syscall.SYS_IOPRIO_GET, ioprioWhoProcess, 0, 0)
	syscall.Syscall(syscall.SYS_IOPRIO_SET, ioprioWhoProcess, 0, ioprioClassIdle<<13)
	defer syscall.Syscall(syscall.SYS_IOPRIO_SET, ioprioWhoProcess, 0, prio)

	croot := l.join(root)
	fd, err := openNoFollow(croot, true)
	if err != nil {
		return err
	}
	d := os.NewFile(uintptr(fd), croot)
	defer d.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	w := walker{ctx: ctx, id: id, inherit: inherit, skip: skip, dev: st.Dev}
	if perSecond > 0 {
		w.tick = time.NewTicker(time.Second / time.Duration(perSecond))
		defer w.tick.Stop()
	}
	if err := w.set(fd, true); err != nil {
		return err
	}
	return w.dir(d, root)
}

type walker struct {
	ctx     context.Context
	id      uint32
	inherit bool
	skip    map[string]bool
	dev     uint64
	tick    *time.Ticker
}

func (w *walker) wait() error {
	if w.tick != nil {
		select {
		case <-w.tick.C:
		case <-w.ctx.Done():
		}
	}
	return w.ctx.Err()
}

func (w *walker) set(fd int, dir bool) error {
	var x fsxattr
	if err := ioctl(fd, fsIOCGetXattr, unsafe.Pointer(&x)); err != nil {
		return err
	}
	x.Projid = w.id
	if dir {
		if w.inherit {
			x.Xflags |= fsXflagProjInherit
		} else {
			x.Xflags &^= fsXflagProjInherit
		}
	}
	return ioctl(fd, fsIOCSetXattr, unsafe.Pointer(&x))
}

// open opens a child of d without following symlinks. mode is the expected
// file type; a changed entry (deleted, replaced, now a symlink) gives ok=false.
func (w *walker) open(d *os.File, name string, mode uint32) (f *os.File, ok bool, err error) {
	flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC | syscall.O_NONBLOCK
	if mode == syscall.S_IFDIR {
		flags |= syscall.O_DIRECTORY
	}
	fd, err := syscall.Openat(int(d.Fd()), name, flags, 0)
	if err != nil {
		if changed(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	f = os.NewFile(uintptr(fd), filepath.Join(d.Name(), name))
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != mode || st.Dev != w.dev {
		f.Close()
		return nil, false, nil
	}
	return f, true, nil
}

func (w *walker) dir(d *os.File, hpath string) error {
	for {
		es, rerr := d.ReadDir(256)
		for _, e := range es {
			if err := w.wait(); err != nil {
				return err
			}
			h := filepath.Join(hpath, e.Name())
			var mode uint32
			switch t := e.Type(); {
			case t.IsDir():
				if w.skip[h] {
					continue
				}
				mode = syscall.S_IFDIR
			case t.IsRegular():
				mode = syscall.S_IFREG
			default:
				continue
			}
			c, ok, err := w.open(d, e.Name(), mode)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			err = w.set(int(c.Fd()), mode == syscall.S_IFDIR)
			if err == nil && mode == syscall.S_IFDIR {
				err = w.dir(c, h)
			}
			c.Close()
			if err != nil {
				return err
			}
		}
		if rerr != nil {
			// ENOENT: the directory was deleted while being read.
			if rerr == io.EOF || errors.Is(rerr, syscall.ENOENT) {
				return nil
			}
			return rerr
		}
	}
}

// changed reports an open error that means the entry is not what ReadDir saw.
func changed(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP)
}

// SelfContainerID reads the own container ID, falling back to the hostname.
func SelfContainerID() string {
	mi, _ := os.ReadFile("/proc/self/mountinfo")
	cg, _ := os.ReadFile("/proc/self/cgroup")
	if id := ContainerIDFrom(string(mi), string(cg)); id != "" {
		return id
	}
	h, _ := os.Hostname()
	return h
}
