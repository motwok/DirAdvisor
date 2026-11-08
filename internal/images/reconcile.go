// SPDX-License-Identifier: AGPL-3.0-or-later

package images

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/images/state"
	"github.com/motwok/HatchLens/internal/ref"
)

const (
	labelEnable = config.LabelEnable
	labelSelf   = config.LabelSelf
	swarmTask   = "com.docker.swarm.task.id"
)

// observed is what a reconcile read from Docker for one container.
type observed struct {
	c       docker.Container
	written string
	local   bool
	swarm   bool
	r       ref.Ref
	running []string
	pulled  int
}

// Reconcile reads the containers and compares them with the known registry
// digests (spec 5, 6, 8). It returns ErrDockerTooOld only; other errors are
// counted and keep the last result.
func (c *Core) Reconcile(ctx context.Context) error {
	start := c.now()
	cfg := c.Config()
	obs, err := c.observe(ctx, cfg)
	if errors.Is(err, ErrDockerTooOld) {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		// A reconcile cut off by the shutdown is no failure.
		if ctx.Err() == nil {
			c.recErrors++
			c.log.Warn("reconcile failed", "err", err)
		}
		return nil
	}
	c.apply(cfg, obs)
	c.ready = true
	c.recSeconds = c.now().Sub(start).Seconds()
	c.saveLocked()
	notify(c.checkCh)
	return nil
}

func (c *Core) observe(ctx context.Context, cfg *config.Config) ([]observed, error) {
	if c.dk == nil {
		return nil, errors.New("no Docker socket configured")
	}
	c.mu.Lock()
	versionOK := c.versionOK
	c.mu.Unlock()
	if !versionOK {
		v, reached, err := c.dk.CheckVersion(ctx)
		if err != nil {
			if reached {
				return nil, fmt.Errorf("%w: %v", ErrDockerTooOld, err)
			}
			return nil, err
		}
		c.log.Info("Docker API", "version", v)
		c.mu.Lock()
		c.versionOK = true
		c.mu.Unlock()
	}
	cs, err := c.dk.Containers(ctx)
	if err != nil {
		return nil, err
	}
	images := map[string]docker.Image{}
	refImages := map[string]string{}
	seen := map[string]bool{}
	var out []observed
	for _, ct := range cs {
		swarm := ct.Labels[swarmTask] != ""
		if ct.State == "removing" || swarm && ct.State != "running" {
			continue
		}
		seen[ct.ID] = true
		written, err := c.configImage(ctx, ct.ID)
		if docker.IsNotFound(err) {
			continue // removed since the list
		}
		if err != nil {
			return nil, err
		}
		o := observed{c: ct, written: written, swarm: swarm, pulled: -1}
		o.local = ref.IsImageID(written, ct.ImageID)
		if !o.local {
			o.r = ref.Parse(written)
			if swarm && o.r.Tag != "" && o.r.Digest != "" {
				o.r.Digest = ""
				o.written = written[:strings.LastIndex(written, "@")]
			}
			if excluded(cfg, ct, o.written, o.r.String()) {
				continue
			}
			im, ok := images[ct.ImageID]
			if !ok {
				if im, err = c.dk.Image(ctx, ct.ImageID); docker.IsNotFound(err) {
					// Removed with "docker rmi -f": the ID is the only running digest.
					im = docker.Image{ID: ct.ImageID}
				} else if err != nil {
					return nil, err
				}
				images[ct.ImageID] = im
			}
			o.running = runningDigests(im, ct.ImageID, o.r.Repository())
			if !swarm {
				id, ok := refImages[o.r.String()]
				if !ok {
					if id, err = c.refImage(ctx, o.r.String()); err != nil {
						return nil, err
					}
					refImages[o.r.String()] = id
				}
				o.pulled = 0
				if id != "" && id != ct.ImageID {
					o.pulled = 1
				}
			}
		} else if excluded(cfg, ct, written) {
			continue
		}
		out = append(out, o)
	}
	c.mu.Lock()
	for id := range c.cfgImages {
		if !seen[id] {
			delete(c.cfgImages, id)
		}
	}
	c.mu.Unlock()
	return out, nil
}

// configImage returns Config.Image; it never changes for a container, so it is cached.
func (c *Core) configImage(ctx context.Context, id string) (string, error) {
	c.mu.Lock()
	s, ok := c.cfgImages[id]
	c.mu.Unlock()
	if ok {
		return s, nil
	}
	j, err := c.dk.Container(ctx, id)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.cfgImages[id] = j.Config.Image
	c.mu.Unlock()
	return j.Config.Image, nil
}

// refImage returns the ID of the local image the reference points to, or "".
func (c *Core) refImage(ctx context.Context, r string) (string, error) {
	im, err := c.dk.Image(ctx, r)
	if docker.IsNotFound(err) {
		return "", nil
	}
	return im.ID, err
}

func excluded(cfg *config.Config, ct docker.Container, images ...string) bool {
	return cfg.ExcludedContainer(ct.Name(), ct.Labels, images...)
}

// runningDigests are the RepoDigests of the repository plus the image ID (REF-5).
func runningDigests(im docker.Image, imageID, repo string) []string {
	var out []string
	for _, rd := range im.RepoDigests {
		if r, d, ok := ref.RepoDigest(rd); ok && r == repo {
			out = append(out, d)
		}
	}
	return append(out, imageID)
}

func stateLabel(s string) string {
	switch s {
	case "running", "paused", "restarting":
		return "running"
	}
	return "stopped"
}

// sinceKey is the container name, for Swarm tasks service and slot (CMP-5).
func sinceKey(ct docker.Container) string {
	svc := ct.Labels["com.docker.swarm.service.name"]
	if svc == "" || ct.Labels[swarmTask] == "" {
		return ct.Name()
	}
	rest := strings.TrimPrefix(ct.Labels["com.docker.swarm.task.name"], svc+".")
	slot, _, _ := strings.Cut(rest, ".")
	return "swarm:" + svc + "." + slot
}

// apply computes the view and updates the state; c.mu is held.
func (c *Core) apply(cfg *config.Config, obs []observed) {
	now := c.now().Unix()
	usedRefs := map[string]bool{}
	usedSince := map[string]bool{}
	// since keys reported with update_available 1 and 0 in this reconcile.
	up, down := map[string]bool{}, map[string]bool{}
	// Running digests per key, and the reference of a key that reports 1.
	cur, upRef := map[string][]string{}, map[string]*state.Ref{}
	prev := c.prev
	c.prev = map[string]entry{}
	var view []entry
	for _, o := range obs {
		ct := o.c
		e := entry{
			Container: ct.Name(), Image: o.written, Stack: stack(ct.Labels), State: stateLabel(ct.State),
			Available: -1, Pulled: o.pulled, running: o.running,
		}
		e.Labels = c.labels(cfg, ct, e.Stack, o.written, o.r.String(), o.local)
		switch {
		case o.local:
			e.Mode = "local"
		case o.r.Digest != "":
			e.Ref, e.Mode = o.r.String(), "pinned"
		default:
			e.Ref = o.r.String()
			usedRefs[e.Ref] = true
			rs := c.st.Refs[e.Ref]
			if rs == nil {
				rs = &state.Ref{Due: now}
				c.st.Refs[e.Ref] = rs
			}
			rs.Unused = 0
			sk := state.SinceKey(sinceKey(ct), e.Ref)
			usedSince[sk] = true
			cur[sk] = append(cur[sk], o.running...)
			switch {
			case rs.Digest != "":
				e.Mode = "checked"
				c.compare(&e, rs, sk, now)
				if e.Available == 1 {
					up[sk] = true
					upRef[sk] = rs
				} else {
					down[sk] = true
				}
			case rs.Checked:
				e.Mode = "error"
			default:
				e.Mode = "pending"
			}
		}
		c.logChange(prev, e)
		view = append(view, e)
	}
	// A key with several containers (Swarm start-first) keeps since while one reports 1.
	for sk := range down {
		if !up[sk] {
			delete(c.st.Since, sk)
		}
	}
	// The known registry digest may be older than a new running image (REG-10).
	for sk, d := range cur {
		slices.Sort(d)
		d = slices.Compact(d)
		seen := c.st.Running[sk]
		if seen == nil {
			seen = &state.Running{}
			c.st.Running[sk] = seen
		}
		seen.Unused = 0
		if slices.Equal(seen.Digests, d) {
			continue
		}
		seen.Digests = d
		if rs := upRef[sk]; rs != nil && rs.Reason == "" && rs.Due > now {
			rs.Due = now
		}
	}
	forget := int64(cfg.Images.ForgetAfter.D().Seconds())
	for k, rs := range c.st.Refs {
		if !usedRefs[k] {
			if rs.Unused == 0 {
				rs.Unused = now
			} else if now-rs.Unused >= forget {
				delete(c.st.Refs, k)
			}
		}
	}
	for k, s := range c.st.Since {
		if !usedSince[k] {
			if s.Unused == 0 {
				s.Unused = now
			} else if now-s.Unused >= forget {
				delete(c.st.Since, k)
			}
		}
	}
	for k, s := range c.st.Running {
		if !usedSince[k] {
			if s.Unused == 0 {
				s.Unused = now
			} else if now-s.Unused >= forget {
				delete(c.st.Running, k)
			}
		}
	}
	for k := range c.warned {
		if name, _, _ := strings.Cut(k, "|"); c.prev[name].Container == "" {
			delete(c.warned, k)
		}
	}
	c.view = view
}

// compare sets available and since (CMP-2, CMP-3); apply drops the since of a 0.
func (c *Core) compare(e *entry, rs *state.Ref, sk string, now int64) {
	if slices.Contains(e.running, rs.Digest) {
		e.Available = 0
		return
	}
	e.Available = 1
	s := c.st.Since[sk]
	if s == nil {
		s = &state.Since{Since: now}
		c.st.Since[sk] = s
	}
	s.Unused = 0
	e.Since = s.Since
}

// logChange logs a change against the previous reconcile; c.prev only holds
// the containers of this one.
func (c *Core) logChange(prev map[string]entry, e entry) {
	p, ok := prev[e.Container]
	c.prev[e.Container] = e
	if !ok || p.Available == e.Available && p.Pulled == e.Pulled {
		return
	}
	digest := ""
	if rs := c.st.Refs[e.Ref]; rs != nil {
		digest = rs.Digest
	}
	c.log.Info("container image changed", "container", e.Container, "image", e.Image,
		"update_available", e.Available, "pulled", e.Pulled, "running", strings.Join(e.running, ","),
		"registry_digest", digest)
}

func stack(l map[string]string) string {
	if s := l["com.docker.compose.project"]; s != "" {
		return s
	}
	return l["com.docker.stack.namespace"]
}

// labels resolves the additional labels (LBL-5): allowlist, then self-service,
// then rules; later ones win.
func (c *Core) labels(cfg *config.Config, ct docker.Container, stk, image, normalized string, local bool) map[string]string {
	images := []string{image}
	if !local {
		images = append(images, normalized)
	}
	return cfg.ExtraLabels(ct.Name(), ct.Labels, stk, images, config.IsImageOwn, func(name string) {
		if k := ct.Name() + "|" + name; !c.warned[k] {
			c.warned[k] = true
			c.log.Warn("label skipped", "container", ct.Name(), "label", name)
		}
	})
}
