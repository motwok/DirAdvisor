// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/motwok/DirAdvisor/internal/config"
	"github.com/motwok/DirAdvisor/internal/docker"
	"github.com/motwok/DirAdvisor/internal/host"
	"github.com/motwok/DirAdvisor/internal/state"
)

const (
	reasonPathMissing     = "path_missing"
	reasonForeignID       = "foreign_project_id"
	reasonExhausted       = "project_ids_exhausted"
	reasonExcludedPath    = "excluded_path"
	reasonDuplicatePath   = "duplicate_path"
	reasonUnsupportedDrv  = "unsupported_volume_driver"
	reasonQuotaAccess     = "quota_access_failed"
	reasonRootUnknown     = "docker_root_unknown"
	defaultDockerRoot     = "/var/lib/docker"
	retryFailedWalksAfter = 10 * time.Minute
)

type candidate struct {
	key, source, name, path, pattern string
	bind                             bool
	vol                              *docker.Volume
	created                          string
	order                            int
	stack, owner                     string
	labels                           map[string]string
	reason                           string
}

type snapshot struct {
	ok         bool
	root       string
	volumes    []docker.Volume
	containers []docker.Container
}

func (c *Core) readDocker(ctx context.Context, cfg *config.Config) (snapshot, error) {
	if !cfg.Docker.Enabled || c.dk == nil {
		return snapshot{}, nil
	}
	if !c.versionOK {
		v, reached, err := c.dk.CheckVersion(ctx)
		if reached && err != nil {
			if v != "" {
				return snapshot{}, fmt.Errorf("%w: %v", ErrDockerTooOld, err)
			}
			return snapshot{}, err
		}
		if err != nil {
			return snapshot{}, err
		}
		c.versionOK = true
	}
	info, err := c.dk.Info(ctx)
	if err != nil {
		return snapshot{}, err
	}
	vols, err := c.dk.Volumes(ctx)
	if err != nil {
		return snapshot{}, err
	}
	cs, err := c.dk.Containers(ctx)
	if err != nil {
		return snapshot{}, err
	}
	root := info.DockerRootDir
	if root == "" {
		root = defaultDockerRoot
	}
	return snapshot{ok: true, root: path.Clean(root), volumes: vols, containers: cs}, nil
}

// Reconcile runs one discovery and assignment pass. It returns ErrDockerTooOld
// wrapped when the daemon is too old; other errors are counted.
func (c *Core) Reconcile(ctx context.Context) error {
	start := c.now()
	c.mu.Lock()
	cfg := c.cfg
	c.mu.Unlock()

	snap, derr := c.readDocker(ctx, cfg)
	if derr != nil && errorsIsTooOld(derr) {
		return derr
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if derr != nil {
		c.recErrors++
		c.log.Warn("Docker not reachable", "err", derr)
	}
	if snap.ok {
		c.dockerRoot, c.st.DockerRoot = snap.root, snap.root
	}
	root := c.dockerRoot
	c.rootKnown = !cfg.Docker.Enabled || c.dk == nil || root != ""

	cands, protected, excludedVols := c.candidates(cfg, snap, root)
	c.dedupe(cands)
	c.release(cfg, snap, cands, protected, excludedVols)

	v := view{}
	c.walkable = map[string]bool{}
	c.tops = map[string]bool{}
	for _, cd := range cands {
		c.tops[cd.path] = true
	}
	statusCache := map[string]statusResult{}
	idRange := cfg.Global.ProjectIDs
	now := c.now()
	dockerMount := ""
	if root != "" {
		if dm, err := c.fs.MountOf(root); err == nil {
			dockerMount = dm.Point
		}
	}

	for _, cd := range cands {
		if cd.reason != "" {
			v.unmeas = append(v.unmeas, unmeasurable{cd.path, cd.reason})
			continue
		}
		exists, isDir := c.fs.Stat(cd.path)
		if !exists || !isDir {
			v.unmeas = append(v.unmeas, unmeasurable{cd.path, reasonPathMissing})
			continue
		}
		if !c.rootKnown && cd.source == state.SourceConfig {
			v.unmeas = append(v.unmeas, unmeasurable{cd.path, reasonRootUnknown})
			continue
		}
		m, err := c.fs.MountOf(cd.path)
		if err != nil {
			c.log.Error("finding mount failed", "path", cd.path, "err", err)
			v.unmeas = append(v.unmeas, unmeasurable{cd.path, reasonQuotaAccess})
			continue
		}
		sr, ok := statusCache[m.Point]
		if !ok {
			sr.reason, sr.err = c.fs.QuotaStatus(m)
			statusCache[m.Point] = sr
		}
		if sr.err != nil {
			c.log.Error("checking quota failed", "mountpoint", m.Point, "err", sr.err)
			v.unmeas = append(v.unmeas, unmeasurable{cd.path, reasonQuotaAccess})
			continue
		}
		if sr.reason != "" {
			v.unmeas = append(v.unmeas, unmeasurable{cd.path, sr.reason})
			if host.AdminAction(sr.reason) {
				docker := cd.source == state.SourceVolume || m.Point == dockerMount
				v.addAction(adminAction{m.Point, m.FSType, sr.reason, command(sr.reason, m, cd.path, docker)})
			}
			continue
		}
		e, reason := c.ensureID(cd, m, idRange)
		if reason != "" {
			v.unmeas = append(v.unmeas, unmeasurable{cd.path, reason})
			if reason == reasonExhausted {
				c.log.Error("no free project ID left", "path", cd.path)
			}
			continue
		}
		ev := entryView{Key: cd.key, Path: cd.path, Source: cd.source, ProjectID: e.ProjectID, Mount: m, Assigning: !e.WalkDone}
		if cd.source == state.SourceVolume {
			ev.Volume = cd.name
			if snap.ok {
				o := resolveOwner(*cd.vol, snap.containers, e, c.self)
				e.Owner, e.Stack = o.Owner, o.Stack
				if o.WorkingDir != "" {
					e.WorkingDir = o.WorkingDir
				}
				if o.Used {
					e.LastUsed = now.Unix()
				}
				e.Labels, e.ContainerLabels = volumeLabels(cfg, *cd.vol, cd.path, o, containerLabelsOf(cfg, e))
				ev.State = o.State
			} else {
				ev.State = c.lastState(cd.key)
			}
			ev.Owner, ev.Stack, ev.Labels, ev.LastUsed = e.Owner, e.Stack, e.Labels, e.LastUsed
		} else {
			ev.Owner, ev.Stack, ev.Labels = cd.owner, cd.stack, cd.labels
			e.Owner, e.Stack, e.Labels = cd.owner, cd.stack, cd.labels
		}
		v.entries = append(v.entries, ev)
		c.walkable[cd.key] = true
	}

	sort.Slice(v.entries, func(i, j int) bool { return v.entries[i].Path < v.entries[j].Path })
	sort.Slice(v.unmeas, func(i, j int) bool { return v.unmeas[i].Path < v.unmeas[j].Path })
	v.overlap = overlaps(v.entries)
	c.setResetTargets(v.entries)
	c.view = v
	c.logActions(cfg, now)
	if snap.ok || !cfg.Docker.Enabled {
		c.ready = true
	}
	c.saveLocked()
	c.recSeconds = c.now().Sub(start).Seconds()
	c.wakeWalker()
	return nil
}

type statusResult struct {
	reason string
	err    error
}

func errorsIsTooOld(err error) bool {
	return errors.Is(err, ErrDockerTooOld)
}

func (v *view) addAction(a adminAction) {
	for _, x := range v.actions {
		if x.Mountpoint == a.Mountpoint && x.Action == a.Action {
			return
		}
	}
	v.actions = append(v.actions, a)
}

func (c *Core) lastState(key string) string {
	for _, e := range c.view.entries {
		if e.Key == key {
			return e.State
		}
	}
	return StateStopped
}

func volumeKind(v docker.Volume, volDir, root string) (p string, bind bool, reason string) {
	typ := v.Options["type"]
	switch {
	case v.Driver != "local":
		return path.Clean(v.Mountpoint), false, reasonUnsupportedDrv
	case typ == "nfs" || typ == "nfs4" || typ == "cifs":
		return path.Clean(v.Mountpoint), false, host.RemoteFilesystem
	case typ == "none" && hasBindOption(v.Options["o"]) && v.Options["device"] != "":
		p = path.Clean(v.Options["device"])
		if config.Excluded(p, root) {
			return p, true, reasonExcludedPath
		}
		return p, true, ""
	case typ == "" && strings.HasPrefix(path.Clean(v.Mountpoint), volDir+"/"):
		return path.Clean(v.Mountpoint), false, ""
	}
	return path.Clean(v.Mountpoint), false, reasonUnsupportedDrv
}

func hasBindOption(o string) bool {
	for _, x := range strings.Split(o, ",") {
		if x == "bind" || x == "rbind" {
			return true
		}
	}
	return false
}

// candidates lists all wanted entries. protected are state keys that must not
// be released (glob with missing parent); excludedVols are existing volumes
// matching docker.exclude.volumes.
func (c *Core) candidates(cfg *config.Config, snap snapshot, root string) ([]*candidate, map[string]bool, map[string]bool) {
	var out []*candidate
	protected := map[string]bool{}
	excludedVols := map[string]bool{}
	order := 0
	if cfg.Docker.Enabled && snap.ok {
		volDir := path.Join(root, "volumes")
		for i := range snap.volumes {
			vol := &snap.volumes[i]
			if excludedVolume(cfg, vol.Name) {
				excludedVols[vol.Name] = true
				continue
			}
			p, bind, reason := volumeKind(*vol, volDir, root)
			out = append(out, &candidate{key: state.VolumeKey(vol.Name), source: state.SourceVolume, name: vol.Name,
				path: p, bind: bind, vol: vol, created: vol.CreatedAt, order: order, reason: reason})
			order++
		}
	} else if cfg.Docker.Enabled {
		// Docker unreachable: keep measuring volumes from the state (DIS-13).
		for k, e := range c.st.Entries {
			if e.Source == state.SourceVolume && !e.PendingReset {
				vol := &docker.Volume{Name: e.Name}
				out = append(out, &candidate{key: k, source: state.SourceVolume, name: e.Name, path: e.Path,
					bind: e.BindVolume, vol: vol, order: order})
				order++
			}
		}
	}
	for _, d := range cfg.Directories {
		if !strings.Contains(d.Path, "*") {
			stack, owner, extra := directoryLabels(d.Labels, nil)
			cd := &candidate{key: state.ConfigKey(d.Path), source: state.SourceConfig, path: d.Path, pattern: d.Path,
				order: order, stack: stack, owner: owner, labels: extra}
			if config.Excluded(d.Path, root) {
				cd.reason = reasonExcludedPath
			}
			out = append(out, cd)
			order++
			continue
		}
		matches, parentMissing, err := config.ExpandGlob(d.Path, c.fs.ListDirs, func(p string) bool {
			_, dir := c.fs.Stat(p)
			return dir
		})
		if parentMissing || err != nil {
			if err != nil {
				c.log.Error("expanding glob failed", "path", d.Path, "err", err)
				c.recErrors++
			}
			out = append(out, &candidate{key: "glob:" + d.Path, source: state.SourceConfig,
				path: config.StaticPrefix(d.Path), order: order, reason: reasonPathMissing})
			order++
			for k, e := range c.st.Entries {
				if e.Pattern == d.Path {
					protected[k] = true
				}
			}
			continue
		}
		for _, m := range matches {
			if config.Excluded(m.Path, root) {
				continue
			}
			stack, owner, extra := directoryLabels(d.Labels, m.Captures)
			out = append(out, &candidate{key: state.ConfigKey(m.Path), source: state.SourceConfig, path: m.Path,
				pattern: d.Path, order: order, stack: stack, owner: owner, labels: extra})
			order++
		}
	}
	return out, protected, excludedVols
}

func excludedVolume(cfg *config.Config, name string) bool {
	for _, p := range cfg.Docker.Exclude.Volumes {
		if config.MatchName(p, name) {
			return true
		}
	}
	return false
}

// dedupe applies DIS-9a: one entry per path.
func (c *Core) dedupe(cands []*candidate) {
	byPath := map[string][]*candidate{}
	for _, cd := range cands {
		if cd.reason == "" {
			byPath[cd.path] = append(byPath[cd.path], cd)
		}
	}
	for _, group := range byPath {
		if len(group) < 2 {
			continue
		}
		sort.SliceStable(group, func(i, j int) bool {
			a, b := group[i], group[j]
			if (a.source == state.SourceVolume) != (b.source == state.SourceVolume) {
				return a.source == state.SourceVolume
			}
			if a.source == state.SourceVolume && a.created != b.created {
				return a.created < b.created
			}
			return a.order < b.order
		})
		win := group[0]
		for _, l := range group[1:] {
			l.reason = reasonDuplicatePath
			if l.key == win.key {
				// Glob match and explicit path: same key, same entry.
				continue
			}
			le := c.st.Entries[l.key]
			if le == nil || le.ProjectID == 0 {
				continue
			}
			if we := c.st.Entries[win.key]; we == nil || we.ProjectID == 0 {
				moved := *le
				moved.PendingReset, moved.ResetTo = false, 0
				c.st.Entries[win.key] = &moved
			}
			delete(c.st.Entries, l.key)
		}
	}
}

// release handles state entries that are no longer wanted (spec PID-8).
func (c *Core) release(cfg *config.Config, snap snapshot, cands []*candidate, protected, excludedVols map[string]bool) {
	wanted := map[string]bool{}
	for _, cd := range cands {
		if cd.reason != reasonDuplicatePath {
			wanted[cd.key] = true
		}
	}
	for k, e := range c.st.Entries {
		if wanted[k] || e.PendingReset || protected[k] {
			continue
		}
		switch e.Source {
		case state.SourceVolume:
			if !cfg.Docker.Enabled || !snap.ok {
				continue
			}
			exists, _ := c.fs.Stat(e.Path)
			switch {
			case excludedVols[e.Name] && exists:
				e.PendingReset = true
			case e.BindVolume && exists:
				e.PendingReset = true
			default:
				delete(c.st.Entries, k)
			}
		case state.SourceConfig:
			exists, _ := c.fs.Stat(e.Path)
			switch {
			case exists:
				e.PendingReset = true
			case strings.Contains(e.Pattern, "*"):
				delete(c.st.Entries, k)
			default:
				// Removed from the config but missing: reset once it shows up.
				e.PendingReset = true
			}
		}
	}
}

// ensureID returns the state entry with an ID for a measurable candidate.
func (c *Core) ensureID(cd *candidate, m host.Mount, r config.IDRange) (*state.Entry, string) {
	inRange := func(id uint32) bool { return int64(id) >= r.From && int64(id) <= r.To }
	e := c.st.Entries[cd.key]
	if e != nil && e.PendingReset {
		if e.Path == cd.path {
			e.PendingReset, e.ResetTo, e.WalkDone = false, 0, false
		} else {
			// Same key, other path (volume re-created elsewhere): the reset
			// goes on under its own key, the candidate starts fresh (PID-12).
			if _, busy := c.st.Entries[state.ResetKey(e.Path)]; !busy {
				c.st.Entries[state.ResetKey(e.Path)] = e
			}
			delete(c.st.Entries, cd.key)
			e = nil
		}
	}
	if e != nil && e.ProjectID != 0 {
		if e.WalkDone {
			top, err := c.fs.ProjectID(cd.path)
			if err == nil && top != e.ProjectID {
				if top != 0 && !inRange(top) && !c.st.HoldsID(top, "") {
					return nil, reasonForeignID
				}
				e.WalkDone = false
			}
		}
		c.fillEntry(e, cd)
		return e, ""
	}
	top, err := c.fs.ProjectID(cd.path)
	if err != nil {
		c.log.Error("reading project ID failed", "path", cd.path, "err", err)
		return nil, reasonPathMissing
	}
	id := uint32(0)
	switch {
	case top != 0 && inRange(top) && !c.st.HoldsID(top, cd.key):
		id = top
	case top != 0 && !inRange(top) && !c.st.HoldsID(top, cd.key):
		return nil, reasonForeignID
	default:
		var err error
		if id, err = c.allocate(m, r); err != nil {
			return nil, reasonQuotaAccess
		}
		if id == 0 {
			return nil, reasonExhausted
		}
	}
	if e == nil {
		e = &state.Entry{}
		c.st.Entries[cd.key] = e
	}
	e.ProjectID, e.WalkDone = id, false
	c.fillEntry(e, cd)
	return e, ""
}

func (c *Core) fillEntry(e *state.Entry, cd *candidate) {
	e.Source, e.Name, e.Path, e.Pattern, e.BindVolume = cd.source, cd.name, cd.path, cd.pattern, cd.bind
}

// allocate returns the lowest free ID of the range, or 0 (spec PID-2).
func (c *Core) allocate(m host.Mount, r config.IDRange) (uint32, error) {
	used := c.st.UsedIDs()
	usage, err := c.fs.Usage(context.Background(), m, uint32(r.From))
	if err != nil {
		c.log.Error("reading quotas failed", "mountpoint", m.Point, "err", err)
		return 0, err
	}
	for id := r.From; id <= r.To; id++ {
		u := uint32(id)
		if used[u] {
			continue
		}
		if q, ok := usage[u]; ok && (q.Bytes > 0 || q.Inodes > 0) {
			continue
		}
		return u, nil
	}
	return 0, nil
}

func under(p, dir string) bool {
	return dir == "/" && p != "/" || strings.HasPrefix(p, dir+"/")
}

func overlaps(es []entryView) []overlap {
	var out []overlap
	for _, a := range es {
		for _, b := range es {
			if a.Key != b.Key && under(b.Path, a.Path) {
				out = append(out, overlap{a.Path, b.Path})
			}
		}
	}
	return out
}

// setResetTargets sets ResetTo of pending resets to the enclosing entry's ID.
func (c *Core) setResetTargets(es []entryView) {
	for _, e := range c.st.Entries {
		if !e.PendingReset {
			continue
		}
		best, bestLen := uint32(0), -1
		for _, v := range es {
			if under(e.Path, v.Path) && len(v.Path) > bestLen {
				best, bestLen = v.ProjectID, len(v.Path)
			}
		}
		e.ResetTo = best
	}
}

// command is the step the admin has to do, filled in for the host. docker is
// set when Docker keeps data on the mount and has to be stopped first.
func command(reason string, m host.Mount, p string, docker bool) string {
	stop, start := fmt.Sprintf("stop everything using %s", m.Point), "start it again"
	if docker {
		stop = fmt.Sprintf("stop Docker (systemctl stop docker docker.socket containerd) and everything else using %s", m.Point)
		start = "start Docker again (systemctl start containerd docker)"
	}
	switch reason {
	case host.EnableProjectQuotaFeature:
		if m.Point == "/" {
			return fmt.Sprintf("boot a rescue system, run: tune2fs -O project,quota %s on the unmounted root filesystem, reboot", m.Source)
		}
		return fmt.Sprintf("%s, umount %s, run: tune2fs -O project,quota %s, mount %s, %s (no fstab change needed)",
			stop, m.Point, m.Source, m.Point, start)
	case host.MountWithPrjquota:
		if m.Point == "/" {
			return "add rootflags=prjquota to the kernel command line (e.g. GRUB_CMDLINE_LINUX, then update-grub) and reboot; on XFS v4 with group quota drop group quota first"
		}
		return fmt.Sprintf("add prjquota to the options of %s (%s) in /etc/fstab, %s, umount %s && mount %s (a remount is not enough), %s; on XFS v4 with group quota drop group quota first",
			m.Point, m.Source, stop, m.Point, m.Point, start)
	case host.XFSv4GroupQuotaConflict:
		return fmt.Sprintf("drop group quota on %s or migrate it to XFS v5", m.Point)
	case host.EnableZFSProjectQuota:
		pool, _, _ := strings.Cut(m.Source, "/")
		return "zpool set feature@project_quota=enabled " + pool
	case host.InstallZFSUserland:
		return "install the ZFS tools (zfsutils) on the host"
	case host.UpgradeKernel:
		return "upgrade the kernel to 5.14 or newer"
	case host.FilesystemNotSupported:
		return fmt.Sprintf("move %s to ext4, XFS or ZFS, or stop measuring it (%s is %s)", p, m.Point, m.FSType)
	}
	return ""
}

func (c *Core) logActions(cfg *config.Config, now time.Time) {
	interval := cfg.Global.AdminActionLogInterval.D()
	active := map[string]bool{}
	for _, a := range c.view.actions {
		k := a.Mountpoint + "|" + a.Action
		active[k] = true
		if last, ok := c.logged[k]; ok && now.Sub(last) < interval {
			continue
		}
		c.logged[k] = now
		c.log.Warn("admin action needed", "mountpoint", a.Mountpoint, "fstype", a.FSType, "action", a.Action, "todo", a.Command)
	}
	for k := range c.logged {
		if !active[k] {
			delete(c.logged, k)
		}
	}
}
