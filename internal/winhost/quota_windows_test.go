// SPDX-License-Identifier: AGPL-3.0-or-later

package winhost

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const parts = "major minor  #blocks  name\n\n   8        0     740272 sda\n   8       16      99300 sdb\n"

func sb(features string, inode int) string {
	return "Filesystem volume name:   <none>\nFilesystem features:      has_journal ext_attr " + features +
		"\nInode size:\t          " + map[bool]string{true: "256", false: "128"}[inode == 256] + "\n"
}

type exitErr int

func (e exitErr) Error() string { return "exit" }
func (e exitErr) ExitCode() int { return int(e) }

// fakeWSL answers wsl calls; fail makes the call with that prefix fail.
type fakeWSL struct {
	calls     []string
	partsN    int
	tuned     bool
	features  string
	inode     int
	fail      string
	fsckExit  int
	afterTune string
}

func (f *fakeWSL) run(args ...string) (string, error) {
	c := strings.Join(args, " ")
	f.calls = append(f.calls, c)
	if f.fail != "" && strings.Contains(c, f.fail) {
		return "", errors.New("failed: " + c)
	}
	switch {
	case strings.HasSuffix(c, "/bin/cat /proc/partitions"):
		f.partsN++
		if f.partsN == 1 {
			return parts, nil
		}
		return parts + "   8      112  268435456 sdh\n", nil
	case strings.Contains(c, "tune2fs -l"):
		if f.tuned {
			return sb(f.afterTune, 256), nil
		}
		return sb(f.features, f.inode), nil
	case strings.Contains(c, "e2fsck"):
		if f.fsckExit != 0 {
			return "", exitErr(f.fsckExit)
		}
	case strings.Contains(c, "tune2fs -O"):
		f.tuned = true
	}
	return "", nil
}

func quotaSetup(t *testing.T, f *fakeWSL) string {
	t.Helper()
	sw, se, sx := wsl, elevated, executable
	t.Cleanup(func() { wsl, elevated, executable = sw, se, sx })
	wsl = f.run
	elevated = func() bool { return true }
	executable = func() (string, error) { return `C:\Program Files\HatchLens Host\hatchlens-host.exe`, nil }
	local := t.TempDir()
	vhd := filepath.Join(local, "Docker", "wsl", "data", "ext4.vhdx")
	os.MkdirAll(filepath.Dir(vhd), 0o755)
	os.WriteFile(vhd, nil, 0o644)
	t.Setenv("LOCALAPPDATA", local)
	return vhd
}

func TestEnableQuota(t *testing.T) {
	f := &fakeWSL{features: "extent", inode: 256, afterTune: "extent project quota"}
	vhd := quotaSetup(t, f)
	var out bytes.Buffer
	if err := EnableQuota("", &out); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.calls, "\n")
	for _, want := range []string{
		`--import hatchlens-tune2fs `,
		`C:\Program Files\HatchLens Host\hatchlens-wsl.tar --version 2`,
		"--mount --vhd " + vhd + " --bare",
		"/sbin/e2fsck -f -p /dev/sdh",
		"/usr/sbin/tune2fs -O project,quota /dev/sdh",
		"--unmount " + vhd,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if f.calls[len(f.calls)-1] != "--unregister hatchlens-tune2fs" {
		t.Fatalf("last call %q", f.calls[len(f.calls)-1])
	}
	if !strings.Contains(out.String(), "project quota enabled") {
		t.Fatal(out.String())
	}

	f = &fakeWSL{features: "extent project quota", inode: 256}
	quotaSetup(t, f)
	out.Reset()
	if err := EnableQuota(`D:\x.vhdx`, &out); err != nil || !strings.Contains(out.String(), "already enabled") {
		t.Fatalf("%v %s", err, out.String())
	}

	f = &fakeWSL{features: "extent", inode: 256, afterTune: "extent project quota", fsckExit: 1}
	quotaSetup(t, f)
	if err := EnableQuota("", &out); err != nil {
		t.Fatalf("fsck exit 1 is fine: %v", err)
	}
}

func TestEnableQuotaErrors(t *testing.T) {
	for _, c := range []struct {
		name string
		f    fakeWSL
		want string
	}{
		{"import", fakeWSL{fail: "--import"}, "failed"},
		{"partitions", fakeWSL{fail: "/proc/partitions"}, "failed"},
		{"mount", fakeWSL{fail: "--mount"}, "wsl --shutdown"},
		{"small inodes", fakeWSL{features: "extent", inode: 128}, "inode size 128"},
		{"list", fakeWSL{fail: "tune2fs -l"}, "failed"},
		{"fsck", fakeWSL{features: "extent", inode: 256, fsckExit: 4}, "exit"},
		{"tune", fakeWSL{features: "extent", inode: 256, fail: "tune2fs -O"}, "failed"},
		{"not set", fakeWSL{features: "extent", inode: 256, afterTune: "extent project"}, "features not set"},
	} {
		f := c.f
		quotaSetup(t, &f)
		if err := EnableQuota("", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	f := &fakeWSL{}
	quotaSetup(t, f)
	f2 := &fakeWSL{}
	n := 0
	wsl = func(args ...string) (string, error) {
		if strings.HasSuffix(strings.Join(args, " "), "/proc/partitions") {
			n++
			if n == 2 {
				return "", errors.New("second read")
			}
		}
		return f2.run(args...)
	}
	if err := EnableQuota("", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "second read") {
		t.Fatal(err)
	}
	wsl = func(args ...string) (string, error) {
		if strings.HasSuffix(strings.Join(args, " "), "/proc/partitions") {
			return parts, nil
		}
		return "", nil
	}
	if err := EnableQuota("", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "one new disk") {
		t.Fatal(err)
	}

	elevated = func() bool { return false }
	if err := EnableQuota("", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatal(err)
	}
	elevated = func() bool { return true }
	t.Setenv("LOCALAPPDATA", t.TempDir())
	if err := EnableQuota("", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "pass -vhd") {
		t.Fatal(err)
	}
	executable = func() (string, error) { return "", errors.New("no exe") }
	if err := EnableQuota(`D:\x.vhdx`, &bytes.Buffer{}); err == nil {
		t.Fatal("executable")
	}
	executable = os.Executable
	t.Setenv("TMP", filepath.Join(t.TempDir(), "missing"))
	if err := EnableQuota(`D:\x.vhdx`, &bytes.Buffer{}); err == nil {
		t.Fatal("temp dir")
	}
}

func TestDesktopDisk(t *testing.T) {
	local := t.TempDir()
	if _, err := desktopDisk(local); err == nil {
		t.Fatal("none")
	}
	for _, p := range []string{`data\ext4.vhdx`, `disk\docker_data.vhdx`} {
		f := filepath.Join(local, "Docker", "wsl", p)
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, nil, 0o644)
	}
	if _, err := desktopDisk(local); err == nil || !strings.Contains(err.Error(), "several") {
		t.Fatal(err)
	}
}

func TestRunWSL(t *testing.T) {
	saved := wslExe
	defer func() { wslExe = saved }()
	wslExe = "cmd.exe"
	elevated()
	if out, err := runWSL("/c", "echo", "hi"); err != nil || out != "hi" {
		t.Fatalf("%q %v", out, err)
	}
	_, err := runWSL("/c", "exit", "3")
	if exitCode(err) != 3 {
		t.Fatal(err)
	}
	if exitCode(errors.New("x")) != -1 {
		t.Fatal("no exit code")
	}
}

func TestMainEnableQuota(t *testing.T) {
	f := &fakeWSL{features: "extent project quota", inode: 256}
	quotaSetup(t, f)
	var out, errb bytes.Buffer
	if Main([]string{"enable-quota", "-vhd", `D:\x.vhdx`}, &out, &errb) != 0 {
		t.Fatal(errb.String())
	}
	if Main([]string{"enable-quota", "-x"}, &out, &errb) != 2 || Main([]string{"enable-quota", "y"}, &out, &errb) != 2 {
		t.Fatal("usage")
	}
	elevated = func() bool { return false }
	if Main([]string{"enable-quota"}, &out, &errb) != 1 {
		t.Fatal("error")
	}
}
