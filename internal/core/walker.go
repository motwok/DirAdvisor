// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"path"
	"sort"
	"time"

	"github.com/motwok/DirAdvisor/internal/state"
)

// Replaced in tests.
var walkerPoll = time.Minute

var excludedTrees = []string{"/var/lib/docker", "/var/lib/containerd", "/proc", "/sys", "/dev", "/run"}

type walkJob struct {
	key     string
	path    string
	id      uint32
	inherit bool
	reset   bool
	skip    map[string]bool
	rate    int
}

// RunWalker processes assignment and reset walks one at a time until ctx ends.
func (c *Core) RunWalker(ctx context.Context) {
	for {
		job, ok := c.nextJob()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
			case <-time.After(walkerPoll):
			}
			continue
		}
		c.log.Info("walk started", "path", job.path, "project_id", job.id, "reset", job.reset)
		err := c.fs.Walk(ctx, job.path, job.id, job.inherit, job.skip, job.rate)
		if ctx.Err() != nil {
			c.log.Info("walk stopped", "path", job.path)
			return
		}
		c.finishJob(job, err)
	}
}

func (c *Core) nextJob() (walkJob, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.st.Entries))
	for k := range c.st.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	now := c.now()
	pick := func(reset bool) (walkJob, bool) {
		for _, k := range keys {
			e := c.st.Entries[k]
			if t, ok := c.failed[k]; ok && now.Sub(t) < retryFailedWalksAfter {
				continue
			}
			if reset && e.PendingReset {
				if exists, dir := c.fs.Stat(e.Path); !exists || !dir {
					continue
				}
				return c.job(k, e, e.ResetTo, e.ResetTo != 0, true), true
			}
			if !reset && !e.PendingReset && !e.WalkDone && c.walkable[k] {
				return c.job(k, e, e.ProjectID, true, false), true
			}
		}
		return walkJob{}, false
	}
	if j, ok := pick(true); ok {
		return j, true
	}
	return pick(false)
}

func (c *Core) job(key string, e *state.Entry, id uint32, inherit, reset bool) walkJob {
	skip := map[string]bool{}
	for k, o := range c.st.Entries {
		if k != key && o.Path != e.Path && under(o.Path, e.Path) {
			skip[o.Path] = true
		}
	}
	for _, t := range append([]string{c.dockerRoot}, excludedTrees...) {
		if t != "" && under(path.Clean(t), e.Path) {
			skip[path.Clean(t)] = true
		}
	}
	return walkJob{key: key, path: e.Path, id: id, inherit: inherit, reset: reset, skip: skip,
		rate: c.cfg.Global.WalkFilesPerSecond}
}

func (c *Core) finishJob(j walkJob, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.st.Entries[j.key]
	if err != nil {
		c.failed[j.key] = c.now()
		c.log.Error("walk failed", "path", j.path, "err", err)
		return
	}
	delete(c.failed, j.key)
	c.log.Info("walk finished", "path", j.path, "project_id", j.id, "reset", j.reset)
	if e == nil {
		return
	}
	if j.reset {
		if e.PendingReset {
			delete(c.st.Entries, j.key)
		}
	} else if e.ProjectID == j.id && !e.PendingReset {
		e.WalkDone = true
		for i := range c.view.entries {
			if c.view.entries[i].Key == j.key {
				c.view.entries[i].Assigning = false
			}
		}
	}
	c.saveLocked()
}
