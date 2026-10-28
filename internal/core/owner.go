// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"sort"

	"github.com/motwok/DirAdvisor/internal/docker"
	"github.com/motwok/DirAdvisor/internal/state"
)

const (
	StateRunning = "running"
	StateStopped = "stopped"
	StateUnused  = "unused"

	composeProject    = "com.docker.compose.project"
	composeWorkingDir = "com.docker.compose.project.working_dir"
)

type ownership struct {
	Owner      string
	Stack      string
	State      string
	OwnerC     *docker.Container
	WorkingDir string
	Used       bool
}

// resolveOwner applies spec section 7 to one volume. self is the own
// container ID (may be empty).
func resolveOwner(vol docker.Volume, cs []docker.Container, prev *state.Entry, self string) ownership {
	var writers []*docker.Container
	var o ownership
	for i := range cs {
		c := &cs[i]
		for _, m := range c.Mounts {
			if m.Type != "volume" || m.Name != vol.Name {
				continue
			}
			if c.State == "running" {
				o.Used = true
			}
			if isSelf(c, self) {
				o.State = StateRunning
				continue
			}
			if m.RW {
				writers = append(writers, c)
			}
		}
	}
	prevOwner, prevStack := "", ""
	if prev != nil {
		prevOwner, prevStack = prev.Owner, prev.Stack
	}
	if o.State == StateRunning {
		// Own data volume: no owner.
		o.Stack = vol.Labels[composeProject]
		return o
	}
	if label := vol.Labels[composeProject]; label != "" {
		o.Stack = label
		var ws []*docker.Container
		for _, c := range writers {
			if c.Labels[composeProject] == label {
				ws = append(ws, c)
			}
		}
		if len(ws) > 0 {
			o.OwnerC = firstUser(ws, prevOwner)
			o.Owner = o.OwnerC.Name()
		} else if prevStack == label {
			o.Owner = prevOwner
		}
	} else if len(writers) > 0 {
		stacks := map[string]bool{}
		for _, c := range writers {
			stacks[c.Labels[composeProject]] = true
		}
		if len(stacks) > 1 {
			o.State = StateUnused
			return o
		}
		o.OwnerC = firstUser(writers, prevOwner)
		o.Owner = o.OwnerC.Name()
		o.Stack = o.OwnerC.Labels[composeProject]
	} else {
		o.Owner, o.Stack = prevOwner, prevStack
	}
	if o.OwnerC == nil && o.Owner != "" {
		for i := range cs {
			if cs[i].Name() == o.Owner {
				o.OwnerC = &cs[i]
			}
		}
	}
	if o.OwnerC != nil {
		o.WorkingDir = o.OwnerC.Labels[composeWorkingDir]
	}
	switch {
	case o.OwnerC != nil && o.OwnerC.State == "running":
		o.State = StateRunning
	case o.Owner != "" || o.Stack != "":
		o.State = StateStopped
	default:
		o.State = StateUnused
	}
	return o
}

func isSelf(c *docker.Container, self string) bool {
	return self != "" && (c.ID == self || len(self) >= 12 && len(c.ID) >= len(self) && c.ID[:len(self)] == self)
}

// firstUser keeps the remembered owner while it writes, else takes the oldest.
func firstUser(ws []*docker.Container, prevOwner string) *docker.Container {
	for _, c := range ws {
		if prevOwner != "" && c.Name() == prevOwner {
			return c
		}
	}
	sorted := append([]*docker.Container{}, ws...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Created != sorted[j].Created {
			return sorted[i].Created < sorted[j].Created
		}
		return sorted[i].Name() < sorted[j].Name()
	})
	return sorted[0]
}
