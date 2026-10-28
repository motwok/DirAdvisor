// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"strconv"

	"github.com/motwok/DirAdvisor/internal/host"
	"github.com/motwok/DirAdvisor/internal/metrics"
)

// BuildInfo describes the binary for diradvisor_build_info.
type BuildInfo struct {
	Version, Revision, GoVersion string
	BuildTime                    int64
}

func m(name, help, typ string) metrics.Family {
	return metrics.Family{Name: "diradvisor_" + name, Help: help, Type: typ}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Collect reads the quotas and returns all metric families (spec section 11).
func (c *Core) Collect(ctx context.Context, bi BuildInfo) []metrics.Family {
	c.mu.Lock()
	v := c.view
	v.entries = append([]entryView(nil), c.view.entries...)
	recErrors, recSeconds, reloadOK := c.recErrors, c.recSeconds, c.reloadOK
	img := c.img
	c.mu.Unlock()

	type group struct {
		m     host.Mount
		minID uint32
	}
	groups := map[string]*group{}
	for _, e := range v.entries {
		k := e.Mount.Point + "|" + e.Mount.Source
		g := groups[k]
		if g == nil {
			g = &group{m: e.Mount, minID: e.ProjectID}
			groups[k] = g
		}
		if e.ProjectID < g.minID {
			g.minID = e.ProjectID
		}
	}
	usage := map[string]map[uint32]host.Usage{}
	for k, g := range groups {
		u, err := c.fs.Usage(ctx, g.m, g.minID)
		if err != nil {
			c.log.Error("reading quotas failed", "mountpoint", g.m.Point, "err", err)
			continue
		}
		usage[k] = u
	}

	bytesF := m("directory_bytes", "Allocated bytes of the directory tree.", "gauge")
	inodesF := m("directory_inodes", "Inodes of the directory tree.", "gauge")
	infoF := m("directory_info", "Changing properties of a measured directory.", "gauge")
	lastF := m("directory_last_used_timestamp_seconds", "Last time a running container used the volume.", "gauge")
	assignF := m("directory_assigning", "1 while the project ID walk of the directory runs.", "gauge")
	for _, e := range v.entries {
		assignF.Samples = append(assignF.Samples, metrics.Sample{Labels: map[string]string{"path": e.Path}, Value: b2f(e.Assigning)})
		infoF.Samples = append(infoF.Samples, metrics.Sample{Labels: map[string]string{
			"path": e.Path, "owner": e.Owner, "state": e.State, "project_id": strconv.FormatUint(uint64(e.ProjectID), 10),
		}, Value: 1})
		if e.Source == "volume" && e.LastUsed > 0 {
			lastF.Samples = append(lastF.Samples, metrics.Sample{Labels: map[string]string{"path": e.Path}, Value: float64(e.LastUsed)})
		}
		u, ok := usage[e.Mount.Point+"|"+e.Mount.Source]
		if !ok {
			continue
		}
		q := u[e.ProjectID]
		l := map[string]string{}
		for k, val := range e.Labels {
			l[k] = val
		}
		l["path"], l["source"], l["volume"], l["stack"] = e.Path, e.Source, e.Volume, e.Stack
		bytesF.Samples = append(bytesF.Samples, metrics.Sample{Labels: l, Value: float64(q.Bytes)})
		inodesF.Samples = append(inodesF.Samples, metrics.Sample{Labels: l, Value: float64(q.Inodes)})
	}
	unmeasF := m("directory_unmeasurable", "1 if a directory cannot be measured.", "gauge")
	for _, u := range v.unmeas {
		unmeasF.Samples = append(unmeasF.Samples, metrics.Sample{Labels: map[string]string{"path": u.Path, "reason": u.Reason}, Value: 1})
	}
	overlapF := m("directory_overlap", "1 per entry inside another entry.", "gauge")
	for _, o := range v.overlap {
		overlapF.Samples = append(overlapF.Samples, metrics.Sample{Labels: map[string]string{"path": o.Outer, "inner": o.Inner}, Value: 1})
	}
	actionF := m("admin_action_needed", "1 per step only the admin can do; see the log for the command.", "gauge")
	for _, a := range v.actions {
		actionF.Samples = append(actionF.Samples, metrics.Sample{Labels: map[string]string{
			"mountpoint": a.Mountpoint, "fstype": a.FSType, "action": a.Action,
		}, Value: 1})
	}
	one := func(f metrics.Family, val float64) metrics.Family {
		f.Samples = []metrics.Sample{{Value: val}}
		return f
	}
	fams := []metrics.Family{
		bytesF, inodesF, infoF, lastF, assignF, unmeasF, overlapF, actionF,
		one(m("reconcile_duration_seconds", "Duration of the last reconcile.", "gauge"), recSeconds),
		one(m("reconcile_errors_total", "Failed reconciles.", "counter"), recErrors),
		one(m("config_last_reload_successful", "1 if the last config load succeeded.", "gauge"), b2f(reloadOK)),
		{Name: "diradvisor_build_info", Help: "Build information.", Type: "gauge", Samples: []metrics.Sample{{Labels: map[string]string{
			"version": bi.Version, "revision": bi.Revision, "goversion": bi.GoVersion}, Value: 1}}},
		one(m("build_timestamp_seconds", "Build time of the binary.", "gauge"), float64(bi.BuildTime)),
		{Name: "diradvisor_image_update_check", Help: "Mode of the image update check.", Type: "gauge",
			Samples: []metrics.Sample{{Labels: map[string]string{"mode": img.mode()}, Value: 1}}},
	}
	if img.mode() == "latest" && img.checked {
		fams = append(fams,
			one(m("image_update_available", "1 if a newer :latest exists than the running image.", "gauge"), b2f(img.available)),
			one(m("image_check_success", "1 if the last image check succeeded.", "gauge"), b2f(img.success)),
		)
		if img.available {
			fams = append(fams, one(m("image_update_available_since_timestamp_seconds",
				"Time the newer image was first seen.", "gauge"), float64(img.since)))
		}
	}
	return fams
}
