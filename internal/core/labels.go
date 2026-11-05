// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"strings"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/state"
)

const selfServicePrefix = config.LabelSelf

func isOwnLabel(n string) bool {
	for _, o := range config.OwnLabels {
		if n == o {
			return true
		}
	}
	return false
}

// volumeLabels resolves the additional labels of a volume (spec LBL-2). Names
// from Docker labels are only sanitized, so a name that is still invalid
// (starts with a digit or "__", or is empty) is dropped here (LBL-3). ctr are
// the labels the owner container contributed; prevCtr replaces them while no
// owner container exists (LBL-4).
func volumeLabels(cfg *config.Config, vol docker.Volume, path string, o ownership, prevCtr map[string]string) (out, ctr map[string]string) {
	out, ctr = map[string]string{}, map[string]string{}
	set := func(k, v string) {
		if v != "" && !isOwnLabel(k) && config.ValidLabelName(k) {
			out[k] = v
		}
	}
	fromContainer := func(k, v string) {
		if v != "" && !isOwnLabel(k) && config.ValidLabelName(k) {
			ctr[k] = v
		}
	}
	// Lowest priority first, later sources overwrite.
	if o.OwnerC != nil {
		for _, r := range cfg.ContainerLabels {
			fromContainer(r.Target("container_label_"), o.OwnerC.Labels[r.From])
		}
	} else {
		for k, v := range prevCtr {
			fromContainer(k, v)
		}
	}
	for k, v := range ctr {
		if strings.HasPrefix(k, "container_label_") || isAllowlistTarget(cfg.ContainerLabels, k) {
			set(k, v)
		}
	}
	for _, r := range cfg.VolumeLabels {
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
				fromContainer(config.SanitizeLabelName(strings.TrimPrefix(k, selfServicePrefix)), v)
			}
		}
	}
	for k, v := range ctr {
		if !strings.HasPrefix(k, "container_label_") && !isAllowlistTarget(cfg.ContainerLabels, k) {
			set(k, v)
		}
	}
	for k, v := range cfg.RuleLabels(config.Subject{Stack: config.Opt(o.Stack), Container: config.Opt(o.Owner), Volume: &vol.Name, Path: &path}) {
		set(k, v)
	}
	return out, ctr
}

// containerLabelsOf returns the remembered container labels; states written
// before they were stored separately get the allowlisted ones from Labels.
func containerLabelsOf(cfg *config.Config, e *state.Entry) map[string]string {
	if e.ContainerLabels != nil || e.Labels == nil {
		return e.ContainerLabels
	}
	out := map[string]string{}
	for k, v := range e.Labels {
		if strings.HasPrefix(k, "container_label_") || isAllowlistTarget(cfg.ContainerLabels, k) {
			out[k] = v
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

// withRules adds the labels of matching rules to a configured directory; the
// directory's own labels win.
func withRules(cfg *config.Config, stack, path string, own map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range cfg.RuleLabels(config.Subject{Stack: config.Opt(stack), Path: &path}) {
		if v != "" {
			out[k] = v
		}
	}
	for k, v := range own {
		out[k] = v
	}
	return out
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
