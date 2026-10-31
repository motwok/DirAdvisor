// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"strings"
	"time"
)

type imageState struct {
	enabled   bool
	known     bool
	pinned    bool
	checked   bool
	available bool
	success   bool
	since     int64
}

func (s imageState) mode() string {
	switch {
	case !s.enabled:
		return "disabled"
	case !s.known:
		return "unknown"
	case s.pinned:
		return "pinned"
	}
	return "latest"
}

// splitRef returns repository and tag ("latest" if none) and whether the
// reference is pinned by digest.
func splitRef(ref string) (repo, tag string, digest bool) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], "", true
	}
	slash := strings.LastIndex(ref, "/")
	if i := strings.LastIndex(ref, ":"); i > slash {
		return ref[:i], ref[i+1:], false
	}
	return ref, "latest", false
}

// CheckImage runs one image update check (spec section 14).
func (c *Core) CheckImage(ctx context.Context) {
	cfg := c.Config()
	c.pub.mu.Lock()
	s := c.pub.img
	c.pub.mu.Unlock()

	s.enabled = cfg.Global.ImageUpdateCheck.Enabled
	if !s.enabled || c.dk == nil || c.self == "" {
		s.known = false
		c.setImage(s)
		return
	}
	cj, err := c.dk.Container(ctx, c.self)
	if err != nil {
		s.known = false
		c.setImage(s)
		return
	}
	s.known = true
	repo, tag, digest := splitRef(cj.Config.Image)
	s.pinned = digest || tag != "latest"
	if s.pinned {
		c.setImage(s)
		return
	}
	s.checked = true
	running, err := c.dk.ImageRepoDigests(ctx, cj.Image)
	var remote string
	if err == nil {
		remote, err = c.dk.DistributionDigest(ctx, repo+":latest")
	}
	if err != nil {
		s.success = false
		c.log.Warn("image update check failed", "err", err)
		c.setImage(s)
		return
	}
	s.success = true
	same := false
	for _, d := range running {
		r, dg, ok := strings.Cut(d, "@")
		if ok && normalizeRepo(r) == normalizeRepo(repo) && dg == remote {
			same = true
		}
	}
	if same {
		s.available, s.since = false, 0
	} else if !s.available {
		s.available, s.since = true, c.now().Unix()
	}
	c.setImage(s)
}

func normalizeRepo(r string) string {
	return strings.TrimPrefix(strings.TrimPrefix(r, "docker.io/"), "library/")
}

func (c *Core) setImage(s imageState) {
	c.pub.mu.Lock()
	c.pub.img = s
	c.pub.mu.Unlock()
}

// RunImageCheck checks at start and then every interval until ctx ends.
func (c *Core) RunImageCheck(ctx context.Context) {
	for {
		c.CheckImage(ctx)
		interval := c.Config().Global.ImageUpdateCheck.Interval.D()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
