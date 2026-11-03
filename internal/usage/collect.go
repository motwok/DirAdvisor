// SPDX-License-Identifier: AGPL-3.0-or-later

package usage

import (
	"errors"
	"io/fs"
	"maps"
	"strings"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/metrics"
)

func fam(name, help, typ string) *metrics.Family {
	return &metrics.Family{Name: "hatchlens_container_" + name, Help: help, Type: typ}
}

func add(f *metrics.Family, l map[string]string, v float64) {
	f.Samples = append(f.Samples, metrics.Sample{Labels: l, Value: v})
}

// read returns the counters of one container from cgroups and /proc (Linux)
// or from the last stats poll (Windows).
func (c *Core) read(t target, cgroupV2 bool) counters {
	var v counters
	if c.windows {
		fromStats(t, &v)
		return v
	}
	if t.pid <= 0 {
		return v
	}
	if cgroupV2 {
		dir, err := c.cgroupDir(t)
		if err == nil {
			readCgroup(dir, &v)
		} else if !errors.Is(err, fs.ErrNotExist) {
			c.warnOnce(t.id+"|cgroup", "container cgroup not found", "container", t.name, "err", err)
		}
	}
	if !t.noNet && c.owns(t.pid, t.id) {
		v.nets = c.readNet(t)
	}
	return v
}

// fromStats maps a Windows stats answer; the network key is the endpoint ID.
func fromStats(t target, v *counters) {
	st := t.stats
	if t.nanoCPUs > 0 {
		v.cpuLimit = float64(t.nanoCPUs) / 1e9
	}
	if t.memLimit > 0 {
		v.memMax = float64(t.memLimit)
	}
	if st == nil {
		return
	}
	v.cpuOK, v.cpu = true, float64(st.CPUStats.CPUUsage.TotalUsage)/1e7
	v.memOK, v.mem, v.workingSet = true, float64(st.MemoryStats.Commit), float64(st.MemoryStats.PrivateWorkingSet)
	s := st.StorageStats
	v.ioOK, v.readBytes, v.writtenBytes = true, float64(s.ReadBytes), float64(s.WriteBytes)
	v.reads, v.writes = float64(s.Reads), float64(s.Writes)
	if t.noNet {
		return
	}
	for key, n := range st.Networks {
		ic := ifaceCounters{iface: key,
			rx: [4]float64{float64(n.RxBytes), float64(n.RxPackets), float64(n.RxErrors), float64(n.RxDropped)},
			tx: [4]float64{float64(n.TxBytes), float64(n.TxPackets), float64(n.TxErrors), float64(n.TxDropped)}}
		for _, nw := range t.networks {
			if nw.endpoint != "" && strings.EqualFold(nw.endpoint, key) {
				ic.network = nw.name
			}
		}
		v.nets = append(v.nets, ic)
	}
}

// Collect returns the families of the running functions (cpu, ram, disk, net).
func (c *Core) Collect() []metrics.Family {
	c.mu.Lock()
	ts := c.targets
	c.mu.Unlock()
	cfg := c.cfg

	cpu := fam("cpu_usage_seconds_total", "CPU time used by the container.", "counter")
	throttled := fam("cpu_throttled_seconds_total", "Time the container was held back by its CPU limit.", "counter")
	cpuLimit := fam("cpu_limit_cores", "CPU limit of the container in cores.", "gauge")
	mem := fam("memory_usage_bytes", "Memory used by the container.", "gauge")
	ws := fam("memory_working_set_bytes", "Memory the container needs, without reclaimable page cache.", "gauge")
	memLimit := fam("memory_limit_bytes", "Memory limit of the container.", "gauge")
	oom := fam("memory_oom_kills_total", "Processes of the container killed for lack of memory.", "counter")
	rb := fam("disk_read_bytes_total", "Bytes read from block devices.", "counter")
	wb := fam("disk_written_bytes_total", "Bytes written to block devices.", "counter")
	rops := fam("disk_reads_total", "Read operations on block devices.", "counter")
	wops := fam("disk_writes_total", "Write operations on block devices.", "counter")
	netNames := []string{"bytes", "packets", "errors", "dropped"}
	var rx, tx [4]*metrics.Family
	for i, n := range netNames {
		rx[i] = fam("network_receive_"+n+"_total", "Received "+n+" per network interface.", "counter")
		tx[i] = fam("network_transmit_"+n+"_total", "Transmitted "+n+" per network interface.", "counter")
	}

	cgroupV2 := !c.windows && c.cgroupV2()
	for _, t := range ts {
		v := c.read(t, cgroupV2)
		l := map[string]string{}
		maps.Copy(l, t.labels)
		l["container"], l["stack"] = t.name, t.stack
		if v.cpuOK {
			add(cpu, l, v.cpu)
			if v.throttledOK {
				add(throttled, l, v.throttled)
			}
		}
		if v.cpuLimit > 0 {
			add(cpuLimit, l, v.cpuLimit)
		}
		if v.memOK {
			add(mem, l, v.mem)
			add(ws, l, v.workingSet)
			if v.oomOK {
				add(oom, l, v.oomKills)
			}
		}
		if v.memMax > 0 {
			add(memLimit, l, v.memMax)
		}
		if v.ioOK {
			add(rb, l, v.readBytes)
			add(wb, l, v.writtenBytes)
			add(rops, l, v.reads)
			add(wops, l, v.writes)
		}
		for _, n := range v.nets {
			nl := maps.Clone(l)
			nl["network"], nl["interface"] = n.network, n.iface
			for i := range netNames {
				add(rx[i], nl, n.rx[i])
				add(tx[i], nl, n.tx[i])
			}
		}
	}

	var out []metrics.Family
	keep := func(f string, fams ...*metrics.Family) {
		if cfg.Runs(f) {
			for _, x := range fams {
				out = append(out, *x)
			}
		}
	}
	keep(config.FnCPU, cpu, throttled, cpuLimit)
	keep(config.FnRAM, mem, ws, memLimit, oom)
	keep(config.FnDisk, rb, wb, rops, wops)
	keep(config.FnNet, rx[:]...)
	keep(config.FnNet, tx[:]...)
	return out
}
