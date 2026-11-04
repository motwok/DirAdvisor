// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"github.com/motwok/HatchLens/internal/config"
	"strconv"
	"sync"
	"time"

	"github.com/motwok/HatchLens/internal/host"
	"github.com/motwok/HatchLens/internal/metrics"
)

// usageTTL is how long one quota read serves scrapes (spec MEA-3).
const usageTTL = 2 * time.Second

type usageGroup struct {
	m     host.Mount
	minID uint32
}

// usageCache shares one quota read between parallel scrapes and reuses it for
// usageTTL, so a scrape storm cannot spawn one zfs process per request.
type usageCache struct {
	mu   sync.Mutex
	at   time.Time
	data map[string]map[uint32]host.Usage
	done chan struct{} // non-nil while a read is in flight
}

// readUsage returns the quota usage per group key, from the cache when fresh.
func (c *Core) readUsage(ctx context.Context, groups map[string]*usageGroup) map[string]map[uint32]host.Usage {
	fresh := func() (map[string]map[uint32]host.Usage, bool) {
		if c.usage.data == nil || c.now().Sub(c.usage.at) >= usageTTL {
			return nil, false
		}
		for k := range groups {
			if _, ok := c.usage.data[k]; !ok {
				return nil, false
			}
		}
		return c.usage.data, true
	}
	c.usage.mu.Lock()
	if d, ok := fresh(); ok {
		c.usage.mu.Unlock()
		return d
	}
	if wait := c.usage.done; wait != nil {
		c.usage.mu.Unlock()
		// A read hung in a ZFS tool must not hold every scrape (WEB-6).
		select {
		case <-wait:
		case <-ctx.Done():
			return nil
		}
		c.usage.mu.Lock()
		if d, ok := fresh(); ok {
			c.usage.mu.Unlock()
			return d
		}
	}
	done := make(chan struct{})
	c.usage.done = done
	c.usage.mu.Unlock()

	data := map[string]map[uint32]host.Usage{}
	for k, g := range groups {
		u, err := c.fs.Usage(ctx, g.m, g.minID)
		if err != nil {
			c.log.Error("reading quotas failed", "mountpoint", g.m.Point, "err", err)
			continue
		}
		data[k] = u
	}
	c.usage.mu.Lock()
	c.usage.data, c.usage.at, c.usage.done = data, c.now(), nil
	c.usage.mu.Unlock()
	close(done)
	return data
}

func m(name, help, typ string) metrics.Family {
	return metrics.Family{Name: "hatchlens_" + name, Help: help, Type: typ}
}

// Stats returns duration and failure count of the reconciles.
func (c *Core) Stats() (seconds, errors float64) {
	c.pub.mu.Lock()
	defer c.pub.mu.Unlock()
	return c.pub.recSeconds, c.pub.recErrors
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Collect reads the quotas and returns the directory metric families (spec section 11).
func (c *Core) Collect(ctx context.Context) []metrics.Family {
	c.pub.mu.Lock()
	v := c.pub.view
	v.entries = append([]entryView(nil), c.pub.view.entries...)
	c.pub.mu.Unlock()

	groups := map[string]*usageGroup{}
	for _, e := range v.entries {
		k := e.Mount.Point + "|" + e.Mount.Source
		g := groups[k]
		if g == nil {
			g = &usageGroup{m: e.Mount, minID: e.ProjectID}
			groups[k] = g
		}
		if e.ProjectID < g.minID {
			g.minID = e.ProjectID
		}
	}
	usage := c.readUsage(ctx, groups)

	bytesF := m("directory_bytes", "Allocated bytes of the directory tree.", "gauge")
	inodesF := m("directory_inodes", "Inodes of the directory tree.", "gauge")
	infoF := m("directory_info", "Changing properties of a measured directory.", "gauge")
	lastF := m("directory_last_used_timestamp_seconds", "Last time a running container used the volume.", "gauge")
	assignF := m("directory_assigning", "1 while the project ID walk of the directory runs.", "gauge")
	for _, e := range v.entries {
		assignF.Samples = append(assignF.Samples, metrics.Sample{Labels: map[string]string{"path": config.Display(e.Path)}, Value: b2f(e.Assigning)})
		infoF.Samples = append(infoF.Samples, metrics.Sample{Labels: map[string]string{
			"path": config.Display(e.Path), "owner": e.Owner, "state": e.State, "project_id": strconv.FormatUint(uint64(e.ProjectID), 10),
		}, Value: 1})
		if e.Source == "volume" && e.LastUsed > 0 {
			lastF.Samples = append(lastF.Samples, metrics.Sample{Labels: map[string]string{"path": config.Display(e.Path)}, Value: float64(e.LastUsed)})
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
		l["path"], l["source"], l["volume"], l["stack"] = config.Display(e.Path), e.Source, e.Volume, e.Stack
		bytesF.Samples = append(bytesF.Samples, metrics.Sample{Labels: l, Value: float64(q.Bytes)})
		inodesF.Samples = append(inodesF.Samples, metrics.Sample{Labels: l, Value: float64(q.Inodes)})
	}
	unmeasF := m("directory_unmeasurable", "1 if a directory cannot be measured.", "gauge")
	for _, u := range v.unmeas {
		unmeasF.Samples = append(unmeasF.Samples, metrics.Sample{Labels: map[string]string{"path": config.Display(u.Path), "reason": u.Reason}, Value: 1})
	}
	overlapF := m("directory_overlap", "1 per entry inside another entry.", "gauge")
	for _, o := range v.overlap {
		overlapF.Samples = append(overlapF.Samples, metrics.Sample{Labels: map[string]string{"path": config.Display(o.Outer), "inner": config.Display(o.Inner)}, Value: 1})
	}
	actionF := m("admin_action_needed", "1 per step only the admin can do; see the log for the command.", "gauge")
	for _, a := range v.actions {
		actionF.Samples = append(actionF.Samples, metrics.Sample{Labels: map[string]string{
			"mountpoint": config.Display(a.Mountpoint), "fstype": a.FSType, "action": a.Action,
		}, Value: 1})
	}
	return []metrics.Family{bytesF, inodesF, infoF, lastF, assignF, unmeasF, overlapF, actionF}
}
