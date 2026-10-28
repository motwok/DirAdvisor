// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"strings"

	"github.com/motwok/DirAdvisor/internal/config"
	"github.com/motwok/DirAdvisor/internal/docker"
)

const selfServicePrefix = "diradvisor.label."

func isOwnLabel(n string) bool {
	for _, o := range config.OwnLabels {
		if n == o {
			return true
		}
	}
	return false
}

// volumeLabels resolves the additional labels of a volume (spec LBL-2).
func volumeLabels(cfg *config.Config, vol docker.Volume, path string, o ownership, prev map[string]string) map[string]string {
	out := map[string]string{}
	set := func(k, v string) {
		if v != "" && !isOwnLabel(k) {
			out[k] = v
		}
	}
	// Lowest priority first, later sources overwrite.
	if o.OwnerC != nil {
		for _, r := range cfg.Docker.ContainerLabels {
			set(r.Target("container_label_"), o.OwnerC.Labels[r.From])
		}
	} else {
		for k, v := range prev {
			if strings.HasPrefix(k, "container_label_") || isAllowlistTarget(cfg.Docker.ContainerLabels, k) {
				set(k, v)
			}
		}
	}
	for _, r := range cfg.Docker.VolumeLabels {
		set(r.Target("volume_label_"), vol.Labels[r.From])
	}
	for k, v := range vol.Labels {
		if strings.HasPrefix(k, selfServicePrefix) {
			set(config.SanitizeLabelName(strings.TrimPrefix(k, selfServicePrefix)), v)
		}
	}
	if o.OwnerC != nil {
		for k, v := range o.OwnerC.Labels {
			if strings.HasPrefix(k, selfServicePrefix) {
				set(config.SanitizeLabelName(strings.TrimPrefix(k, selfServicePrefix)), v)
			}
		}
	}
	for _, rule := range cfg.Docker.Labels {
		if ruleMatches(rule.Match, vol.Name, o, path) {
			for k, v := range rule.Labels {
				set(k, v)
			}
		}
	}
	return out
}

func isAllowlistTarget(refs []config.LabelRef, name string) bool {
	for _, r := range refs {
		if r.To == name {
			return true
		}
	}
	return false
}

func ruleMatches(m config.Match, volume string, o ownership, path string) bool {
	if m.Stack != "" && !config.MatchName(m.Stack, o.Stack) {
		return false
	}
	if m.Volume != "" && !config.MatchName(m.Volume, volume) {
		return false
	}
	if m.Container != "" && !config.MatchName(m.Container, o.Owner) {
		return false
	}
	if m.Path != "" && !config.MatchPath(m.Path, path) {
		return false
	}
	return true
}

// directoryLabels splits configured labels into stack, owner and the rest.
func directoryLabels(labels map[string]string, caps []string) (stack, owner string, extra map[string]string) {
	extra = map[string]string{}
	for k, v := range labels {
		v = config.SubstCaptures(v, caps)
		switch k {
		case "stack":
			stack = v
		case "owner":
			owner = v
		default:
			if v != "" {
				extra[k] = v
			}
		}
	}
	return stack, owner, extra
}
