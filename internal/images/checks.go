// SPDX-License-Identifier: AGPL-3.0-or-later

package images

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/images/state"
	"github.com/motwok/HatchLens/internal/ref"
)

const (
	registryGap  = 10 * time.Second
	firstBackoff = 5 * time.Minute
	idleWait     = time.Minute
)

// backoff is 5 minutes doubled per step, at most limit.
func backoff(step int, limit time.Duration) time.Duration {
	d := firstBackoff
	for i := 0; i < step && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

// auth is the X-Registry-Auth value for a registry, or "" (CFG-9).
func auth(cfg *config.Config, host string) string {
	r, ok := cfg.Images.Registry(host)
	if !ok || r.Password == "" {
		return ""
	}
	addr := host
	if host == "docker.io" {
		addr = "https://index.docker.io/v1/"
	}
	b, _ := json.Marshal(map[string]string{"username": r.Username, "password": r.Password, "serveraddress": addr})
	return base64.URLEncoding.EncodeToString(b)
}

// reason classifies a failed registry check (REG-7).
func reason(err error) string {
	var ae *docker.APIError
	if errors.As(err, &ae) {
		switch ae.Status {
		case 404:
			return "not_found"
		case 401, 403:
			return "denied"
		case 429:
			return "rate_limited"
		}
	}
	return "unreachable"
}

// next picks the due reference to check now, or returns how long to wait (REG-2 to REG-6).
func (c *Core) next(now time.Time) (string, time.Duration) {
	wait := idleWait
	pick, pickDue := "", int64(0)
	for _, e := range c.view {
		rs := c.st.Refs[e.Ref]
		if e.Mode != "pending" && e.Mode != "error" && e.Mode != "checked" || rs == nil {
			continue
		}
		host := ref.Parse(e.Ref).Domain
		at := time.Unix(rs.Due, 0)
		if r := c.st.Registries[host]; r != nil && time.Unix(r.Until, 0).After(at) {
			at = time.Unix(r.Until, 0)
		}
		if t := c.lastReq[host].Add(registryGap); t.After(at) {
			at = t
		}
		if at.After(now) {
			wait = min(wait, at.Sub(now))
			continue
		}
		if pick == "" || rs.Due < pickDue {
			pick, pickDue = e.Ref, rs.Due
		}
	}
	return pick, wait
}

// CheckDue runs at most one due registry check and returns how long to wait
// before the next call.
func (c *Core) CheckDue(ctx context.Context) time.Duration {
	c.mu.Lock()
	if !c.ready || c.dk == nil {
		c.mu.Unlock()
		return idleWait
	}
	cfg := c.cfg
	now := c.now()
	r, wait := c.next(now)
	if r == "" {
		c.mu.Unlock()
		return wait
	}
	host := ref.Parse(r).Domain
	c.lastReq[host] = now
	c.mu.Unlock()

	digest, err := c.dk.Distribution(ctx, r, auth(cfg, host))
	if err == nil && digest == "" {
		err = errors.New("no digest in the answer")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	rs := c.st.Refs[r]
	if rs == nil || ctx.Err() != nil {
		// A check cut off by the shutdown is no answer of the registry.
		return 0
	}
	t := c.now().Unix()
	interval := cfg.Images.CheckInterval.D()
	rs.Checked = true
	if err == nil {
		secs := max(int64(interval.Seconds()), 1)
		if rs.LastSuccess == 0 {
			rs.Due = t + c.rand(secs)
		} else {
			rs.Due = t + secs
		}
		rs.Digest, rs.LastSuccess, rs.Fails, rs.Reason = digest, t, 0, ""
		delete(c.st.Registries, host)
	} else {
		rs.Reason = reason(err)
		rs.Due = t + int64(backoff(rs.Fails, interval).Seconds())
		rs.Fails++
		c.log.Warn("registry check failed", "reference", r, "reason", rs.Reason, "err", err)
		if rs.Reason == "rate_limited" {
			reg := c.st.Registries[host]
			if reg == nil {
				reg = &state.Registry{}
				c.st.Registries[host] = reg
			}
			reg.Until = t + int64(backoff(reg.Step, interval).Seconds())
			reg.Step++
		}
	}
	c.saveLocked()
	notify(c.reconcileCh)
	return 0
}

// RunChecks runs registry checks until ctx ends.
func (c *Core) RunChecks(ctx context.Context) {
	for ctx.Err() == nil {
		wait := c.CheckDue(ctx)
		if wait <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		case <-c.checkCh:
		}
	}
}
