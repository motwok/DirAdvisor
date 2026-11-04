// SPDX-License-Identifier: AGPL-3.0-or-later

package usage

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// counters of one container; a false ok means the source was not readable.
type counters struct {
	cpuOK                   bool
	cpu, cpuLimit           float64 // cpuLimit 0: none
	throttledOK             bool
	throttled               float64
	memOK                   bool
	mem, workingSet, memMax float64 // memMax 0: none
	oomOK                   bool
	oomKills                float64
	ioOK                    bool
	readBytes, writtenBytes float64
	reads, writes           float64
	nets                    []ifaceCounters
}

// ifaceCounters: receive and transmit bytes, packets, errors, dropped.
type ifaceCounters struct {
	network, iface string
	rx, tx         [4]float64
}

func (c *Core) procFile(pid int, name ...string) string {
	return filepath.Join(append([]string{c.root, "proc", strconv.Itoa(pid)}, name...)...)
}

// cgroupOf returns the cgroup v2 path of pid as /proc shows it (relative to the
// own cgroup namespace), or "".
func (c *Core) cgroupOf(pid int) string {
	b, err := os.ReadFile(c.procFile(pid, "cgroup"))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(l, "0::"); ok {
			return p
		}
	}
	return ""
}

// owns reports whether pid still belongs to the container: a cgroup line
// (v2 "0::" or any v1 controller) names it.
func (c *Core) owns(pid int, id string) bool {
	b, err := os.ReadFile(c.procFile(pid, "cgroup"))
	return err == nil && strings.Contains(string(b), id)
}

// ownBase finds the host path of the own cgroup namespace root once: the
// cgroup directory listing the own host PID, minus the own path in the namespace.
func (c *Core) ownBase() (string, error) {
	c.baseMu.Lock()
	defer c.baseMu.Unlock()
	if c.baseDone {
		return c.base, c.baseErr
	}
	c.baseDone = true
	c.baseErr = errors.New("own cgroup not found")
	// cgroup.procs shows PIDs as the reader's PID namespace sees them.
	self := strconv.Itoa(c.selfPID())
	inNS := ""
	if b, err := os.ReadFile(filepath.Join(c.root, "proc", "self", "cgroup")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if p, ok := strings.CutPrefix(l, "0::"); ok {
				inNS = p
			}
		}
	}
	top := filepath.Join(c.root, "sys", "fs", "cgroup")
	filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(top, p)
		if strings.Count(rel, string(filepath.Separator)) > 8 {
			return filepath.SkipDir
		}
		b, _ := os.ReadFile(filepath.Join(p, "cgroup.procs"))
		for _, l := range strings.Fields(string(b)) {
			if l == self {
				found := "/" + filepath.ToSlash(rel)
				if found == "/." {
					found = "/"
				}
				c.base, c.baseErr = path.Clean(strings.TrimSuffix(found, strings.TrimSuffix(inNS, "/"))), nil
				return filepath.SkipAll
			}
		}
		return nil
	})
	return c.base, c.baseErr
}

// cgroupV2 reports whether the host has a cgroup v2 hierarchy; without it CPU,
// memory and disk are not reported (logged once).
func (c *Core) cgroupV2() bool {
	p := filepath.Join(c.root, "sys", "fs", "cgroup", "cgroup.controllers")
	if _, err := os.Stat(p); err != nil {
		c.warnOnce("cgroup2", "no cgroup v2 found, no CPU, memory and disk metrics", "path", p)
		return false
	}
	return true
}

// cgroupDir returns the container's cgroup directory, or "" with a reason.
func (c *Core) cgroupDir(t target) (string, error) {
	p := c.cgroupOf(t.pid)
	if p == "" || !strings.Contains(p, t.id) {
		return "", fs.ErrNotExist
	}
	if strings.HasPrefix(p, "/..") {
		base, err := c.ownBase()
		if err != nil {
			return "", err
		}
		p = path.Join(base, p)
	}
	return filepath.Join(c.root, "sys", "fs", "cgroup", filepath.FromSlash(p)), nil
}

// keyValues parses "key value" lines.
func keyValues(b []byte) map[string]float64 {
	out := map[string]float64{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, " "); ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				out[k] = f
			}
		}
	}
	return out
}

// limit parses cgroup "max" or a number; 0 means none.
func limit(b []byte) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return f
}

// readCgroup fills CPU, memory and I/O from the container's cgroup v2 files.
func readCgroup(dir string, v *counters) {
	read := func(name string) ([]byte, bool) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		return b, err == nil
	}
	if b, ok := read("cpu.stat"); ok {
		kv := keyValues(b)
		v.cpuOK, v.cpu = true, kv["usage_usec"]/1e6
		if t, ok := kv["throttled_usec"]; ok {
			v.throttledOK, v.throttled = true, t/1e6
		}
		if b, ok := read("cpu.max"); ok {
			quota, period, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
			if q, p := limit([]byte(quota)), limit([]byte(period)); q > 0 && p > 0 {
				v.cpuLimit = q / p
			}
		}
	}
	if b, ok := read("memory.current"); ok {
		v.memOK, v.mem = true, limit(b)
		v.workingSet = v.mem
		if b, ok := read("memory.stat"); ok {
			v.workingSet = max(0, v.mem-keyValues(b)["inactive_file"])
		}
		if b, ok := read("memory.max"); ok {
			v.memMax = limit(b)
		}
		if b, ok := read("memory.events"); ok {
			v.oomOK, v.oomKills = true, keyValues(b)["oom_kill"]
		}
	}
	if b, ok := read("io.stat"); ok {
		v.ioOK = true
		for _, l := range strings.Split(string(b), "\n") {
			for _, f := range strings.Fields(l) {
				k, s, ok := strings.Cut(f, "=")
				if !ok {
					continue
				}
				n, _ := strconv.ParseFloat(s, 64)
				switch k {
				case "rbytes":
					v.readBytes += n
				case "wbytes":
					v.writtenBytes += n
				case "rios":
					v.reads += n
				case "wios":
					v.writes += n
				}
			}
		}
	}
}

// readNet reads the interface counters of the container's network namespace
// and maps each interface to a Docker network by the routes (IPv4) and
// addresses (IPv6) in /proc of the host; no capability needed.
func (c *Core) readNet(t target) []ifaceCounters {
	b, err := os.ReadFile(c.procFile(t.pid, "net", "dev"))
	if err != nil {
		return nil
	}
	byIface := map[string]string{}
	routes, _ := os.ReadFile(c.procFile(t.pid, "net", "route"))
	addrs, _ := os.ReadFile(c.procFile(t.pid, "net", "if_inet6"))
	for _, n := range t.networks {
		if i := routeIface(routes, n.ip4, n.prefix4); i != "" {
			byIface[i] = n.name
		} else if i := addrIface(addrs, n.ip6); i != "" {
			byIface[i] = n.name
		}
	}
	var out []ifaceCounters
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if !ok || name == "lo" || len(f) < 16 {
			continue
		}
		num := func(i int) float64 { n, _ := strconv.ParseFloat(f[i], 64); return n }
		out = append(out, ifaceCounters{network: byIface[name], iface: name,
			rx: [4]float64{num(0), num(1), num(2), num(3)}, tx: [4]float64{num(8), num(9), num(10), num(11)}})
	}
	return out
}

// routeIface finds the interface of the on-link route of ip/prefix in /proc/net/route.
func routeIface(routes []byte, ip string, prefix int) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is4() {
		return ""
	}
	pfx := netip.PrefixFrom(a, prefix).Masked()
	want := pfx.Addr().As4()
	for _, l := range strings.Split(string(routes), "\n") {
		f := strings.Fields(l)
		if len(f) < 8 || f[0] == "Iface" {
			continue
		}
		dest, mask := hexAddr(f[1]), hexAddr(f[7])
		if dest == want && maskBits(mask) == prefix {
			return f[0]
		}
	}
	return ""
}

// hexAddr decodes an IPv4 address as /proc/net/route prints it (host byte order).
func hexAddr(s string) [4]byte {
	var out [4]byte
	n, err := strconv.ParseUint(s, 16, 32)
	if err == nil {
		binary.NativeEndian.PutUint32(out[:], uint32(n))
	}
	return out
}

func maskBits(m [4]byte) int {
	bits := 0
	for _, b := range m {
		for ; b&0x80 != 0; b <<= 1 {
			bits++
		}
	}
	return bits
}

// addrIface finds the interface holding ip in /proc/net/if_inet6.
func addrIface(addrs []byte, ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is6() {
		return ""
	}
	raw := a.As16()
	want := hex.EncodeToString(raw[:])
	for _, l := range strings.Split(string(addrs), "\n") {
		if f := strings.Fields(l); len(f) == 6 && f[0] == want {
			return f[5]
		}
	}
	return ""
}
