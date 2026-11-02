// SPDX-License-Identifier: AGPL-3.0-or-later

package images

import (
	"maps"

	"github.com/motwok/HatchLens/internal/metrics"
)

func m(name, help, typ string) metrics.Family {
	return metrics.Family{Name: "hatchlens_" + name, Help: help, Type: typ}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Stats returns duration and failure count of the reconciles.
func (c *Core) Stats() (seconds, errors float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recSeconds, c.recErrors
}

// Collect returns the image metric families from the last reconcile and checks.
func (c *Core) Collect() []metrics.Family {
	c.mu.Lock()
	defer c.mu.Unlock()

	avail := m("container_image_update_available", "1 if the registry has a newer image for the container's reference.", "gauge")
	since := m("container_image_update_available_since_timestamp_seconds", "Time the update became available.", "gauge")
	pulled := m("container_image_update_pulled", "1 if the reference points to another local image than the container runs.", "gauge")
	info := m("container_image_info", "Changing properties of a reported container.", "gauge")
	success := m("image_check_success", "1 if the last registry check of the reference succeeded.", "gauge")
	checkErr := m("image_check_error", "1 while the last registry check of the reference failed.", "gauge")
	stamp := m("image_check_timestamp_seconds", "Time of the last successful registry check.", "gauge")

	refs := map[string]bool{}
	for _, e := range c.view {
		l := map[string]string{}
		maps.Copy(l, e.Labels)
		l["container"], l["image"], l["stack"] = e.Container, e.Image, e.Stack
		if e.Available >= 0 {
			avail.Samples = append(avail.Samples, metrics.Sample{Labels: l, Value: float64(e.Available)})
		}
		if e.Available == 1 {
			since.Samples = append(since.Samples, metrics.Sample{Labels: l, Value: float64(e.Since)})
		}
		if e.Pulled >= 0 {
			pulled.Samples = append(pulled.Samples, metrics.Sample{Labels: l, Value: float64(e.Pulled)})
		}
		info.Samples = append(info.Samples, metrics.Sample{Labels: map[string]string{
			"container": e.Container, "reference": e.Ref, "state": e.State, "mode": e.Mode}, Value: 1})
		if e.Mode == "pending" || e.Mode == "error" || e.Mode == "checked" {
			refs[e.Ref] = true
		}
	}
	for r := range refs {
		rs := c.st.Refs[r]
		if rs == nil || !rs.Checked {
			continue
		}
		l := map[string]string{"reference": r}
		success.Samples = append(success.Samples, metrics.Sample{Labels: l, Value: b2f(rs.Reason == "")})
		if rs.Reason != "" {
			checkErr.Samples = append(checkErr.Samples, metrics.Sample{
				Labels: map[string]string{"reference": r, "reason": rs.Reason}, Value: 1})
		}
		if rs.LastSuccess > 0 {
			stamp.Samples = append(stamp.Samples, metrics.Sample{Labels: l, Value: float64(rs.LastSuccess)})
		}
	}

	return []metrics.Family{avail, since, pulled, info, success, checkErr, stamp}
}
