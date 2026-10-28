// SPDX-License-Identifier: AGPL-3.0-or-later

// Package host accesses the host filesystem: mounts, project IDs, quotas, walks.
package host

import (
	"bufio"
	"context"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Reasons and admin actions (spec section 10).
const (
	EnableProjectQuotaFeature = "enable_project_quota_feature"
	MountWithPrjquota         = "mount_with_prjquota"
	XFSv4GroupQuotaConflict   = "xfs_v4_group_quota_conflict"
	EnableZFSProjectQuota     = "enable_zfs_project_quota"
	InstallZFSUserland        = "install_zfs_userland"
	UpgradeKernel             = "upgrade_kernel"
	FilesystemNotSupported    = "filesystem_not_supported"
	RemoteFilesystem          = "remote_filesystem"
)

// AdminAction reports whether a reason is fixed by the admin on the host.
func AdminAction(reason string) bool {
	switch reason {
	case EnableProjectQuotaFeature, MountWithPrjquota, XFSv4GroupQuotaConflict, EnableZFSProjectQuota,
		InstallZFSUserland, UpgradeKernel, FilesystemNotSupported:
		return true
	}
	return false
}

// Mount is a mounted filesystem; Point is a host path.
type Mount struct {
	Point   string
	FSType  string
	Source  string
	Options string
}

type Usage struct {
	Bytes  uint64
	Inodes uint64
}

type FS interface {
	Stat(p string) (exists, isDir bool)
	ListDirs(p string) ([]string, error)
	MountOf(p string) (Mount, error)
	ProjectID(p string) (uint32, error)
	// QuotaStatus returns "" if project quotas work on m, else a reason.
	QuotaStatus(m Mount) (string, error)
	Usage(ctx context.Context, m Mount, minID uint32) (map[uint32]Usage, error)
	// Walk sets id on every file and directory below root. inherit sets or
	// clears FS_XFLAG_PROJINHERIT on directories. Paths in skip, symlinks and
	// other filesystems are not entered.
	Walk(ctx context.Context, root string, id uint32, inherit bool, skip map[string]bool, perSecond int) error
	// ZFSVersions returns the kernel module and userland versions, if ZFS is used.
	ZFSVersions() (module, userland string)
}

type rawMount struct {
	Point, FSType, Source, Options string
}

// parseMountinfo reads /proc/self/mountinfo lines.
func parseMountinfo(r io.Reader) []rawMount {
	var out []rawMount
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		sep := -1
		for i, x := range f {
			if x == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || sep+3 > len(f) {
			continue
		}
		opts := f[5]
		if sep+3 < len(f) {
			opts += "," + f[sep+3]
		}
		out = append(out, rawMount{Point: unescape(f[4]), FSType: f[sep+1], Source: unescape(f[sep+2]), Options: opts})
	}
	return out
}

func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// longestMount returns the mount containing p (container path).
func longestMount(mounts []rawMount, p string) (rawMount, bool) {
	var best rawMount
	found := false
	for _, m := range mounts {
		if m.Point == p || m.Point == "/" || strings.HasPrefix(p, strings.TrimSuffix(m.Point, "/")+"/") {
			if !found || len(m.Point) >= len(best.Point) {
				best, found = m, true
			}
		}
	}
	return best, found
}

// toHost converts a container path below root to a host path.
func toHost(root, p string) string {
	if root == "/" || root == "" {
		return path.Clean(p)
	}
	if p == root {
		return "/"
	}
	if strings.HasPrefix(p, root+"/") {
		return p[len(root):]
	}
	return path.Clean(p)
}

func hasOption(opts string, names ...string) bool {
	for _, o := range strings.Split(opts, ",") {
		for _, n := range names {
			if o == n {
				return true
			}
		}
	}
	return false
}

var (
	reMountinfoID = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)
	reCgroupID    = regexp.MustCompile(`(?:docker-|/docker/)([0-9a-f]{64})`)
)

// ContainerIDFrom finds the own container ID in mountinfo or cgroup content.
func ContainerIDFrom(mountinfo, cgroup string) string {
	if m := reMountinfoID.FindStringSubmatch(mountinfo); m != nil {
		return m[1]
	}
	if m := reCgroupID.FindStringSubmatch(cgroup); m != nil {
		return m[1]
	}
	return ""
}

// parseProjectspace parses `zfs projectspace -Hp -o name,used,objused`.
func parseProjectspace(out string) map[uint32]Usage {
	m := map[uint32]Usage{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		id, err := strconv.ParseUint(f[0], 10, 32)
		if err != nil {
			continue
		}
		b, _ := strconv.ParseUint(f[1], 10, 64)
		n, _ := strconv.ParseUint(f[2], 10, 64)
		m[uint32(id)] = Usage{Bytes: b, Inodes: n}
	}
	return m
}

// zfsFeatureOK reports whether a zpool feature value means enabled.
func zfsFeatureOK(v string) bool {
	v = strings.TrimSpace(v)
	return v == "enabled" || v == "active"
}
