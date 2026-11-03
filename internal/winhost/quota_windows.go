// SPDX-License-Identifier: AGPL-3.0-or-later

package winhost

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

// Temporary WSL distribution with e2fsprogs; its root file system ships next
// to the executable.
const (
	distro = "hatchlens-tune2fs"
	rootfs = "hatchlens-wsl.tar"
)

// Replaceable in tests.
var (
	wsl        = runWSL
	wslExe     = "wsl.exe"
	elevated   = func() bool { return windows.GetCurrentProcessToken().IsElevated() }
	executable = os.Executable
)

func runWSL(args ...string) (string, error) {
	cmd := exec.Command(wslExe, args...)
	cmd.Env = append(os.Environ(), "WSL_UTF8=1")
	b, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(b))
	if err != nil {
		return out, fmt.Errorf("wsl %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return out, nil
}

// EnableQuota sets the ext4 features project and quota on the data disk of
// Docker Desktop. Docker Desktop and WSL must be stopped.
func EnableQuota(vhd string, out io.Writer) error {
	if !elevated() {
		return errors.New("run as administrator")
	}
	if vhd == "" {
		v, err := desktopDisk(os.Getenv("LOCALAPPDATA"))
		if err != nil {
			return err
		}
		vhd = v
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", distro)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	wsl("--unregister", distro) // left over from an aborted run
	if _, err := wsl("--import", distro, dir, filepath.Join(filepath.Dir(exe), rootfs), "--version", "2"); err != nil {
		return err
	}
	defer wsl("--unregister", distro)
	before, err := partitions()
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "attaching", vhd)
	if _, err := wsl("--mount", "--vhd", vhd, "--bare"); err != nil {
		return fmt.Errorf("%w (quit Docker Desktop and run wsl --shutdown first)", err)
	}
	defer wsl("--unmount", vhd)
	after, err := partitions()
	if err != nil {
		return err
	}
	dev, err := attached(before, after)
	if err != nil {
		return err
	}
	if done, err := ready(dev); err != nil || done {
		if done {
			fmt.Fprintln(out, "project quota is already enabled")
		}
		return err
	}
	fmt.Fprintln(out, "checking", dev)
	if _, err := wsl("-d", distro, "--exec", "/sbin/e2fsck", "-f", "-p", dev); err != nil && exitCode(err) != 1 {
		return err
	}
	fmt.Fprintln(out, "enabling project quota on", dev)
	if _, err := wsl("-d", distro, "--exec", "/usr/sbin/tune2fs", "-O", "project,quota", dev); err != nil {
		return err
	}
	if done, err := ready(dev); err != nil || !done {
		return fmt.Errorf("features not set on %s: %v", dev, err)
	}
	fmt.Fprintln(out, "project quota enabled; start Docker Desktop again")
	return nil
}

// desktopDisk finds the data disk of Docker Desktop: data\ext4.vhdx up to
// 4.29, disk\docker_data.vhdx since.
func desktopDisk(local string) (string, error) {
	base := filepath.Join(local, "Docker", "wsl")
	var found []string
	for _, p := range []string{filepath.Join(base, "data", "ext4.vhdx"), filepath.Join(base, "disk", "docker_data.vhdx")} {
		if _, err := os.Stat(p); err == nil {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no Docker Desktop data disk under %s; pass -vhd", base)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("several data disks (%s); pass -vhd", strings.Join(found, ", "))
}

func partitions() (map[string]bool, error) {
	s, err := wsl("-d", distro, "--exec", "/bin/cat", "/proc/partitions")
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) == 4 && f[0] != "major" {
			names[f[3]] = true
		}
	}
	return names, nil
}

func attached(before, after map[string]bool) (string, error) {
	var added []string
	for n := range after {
		if !before[n] {
			added = append(added, n)
		}
	}
	if len(added) != 1 {
		return "", fmt.Errorf("expected one new disk in WSL, found %v", added)
	}
	return "/dev/" + added[0], nil
}

// ready reports whether dev has both features; it fails on anything that is
// not ext4 with 256-byte inodes.
func ready(dev string) (bool, error) {
	s, err := wsl("-d", distro, "--exec", "/usr/sbin/tune2fs", "-l", dev)
	if err != nil {
		return false, err
	}
	var features []string
	inode := 0
	for _, l := range strings.Split(s, "\n") {
		k, v, _ := strings.Cut(l, ":")
		switch strings.TrimSpace(k) {
		case "Filesystem features":
			features = strings.Fields(v)
		case "Inode size":
			inode, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if inode < 256 {
		return false, fmt.Errorf("%s: inode size %d, project quota needs 256", dev, inode)
	}
	has := map[string]bool{}
	for _, f := range features {
		has[f] = true
	}
	return has["project"] && has["quota"], nil
}

func exitCode(err error) int {
	var e interface{ ExitCode() int }
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	return -1
}
