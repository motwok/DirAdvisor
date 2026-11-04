// SPDX-License-Identifier: AGPL-3.0-or-later

package usage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/metrics"
)

type fakeDocker struct {
	version    string
	reached    bool
	versionErr error
	listErr    error
	cs         []docker.Container
	inspects   map[string]docker.ContainerJSON
	inspectErr map[string]error
	stats      map[string]docker.Stats
	statsErr   map[string]error
	inspected  int
}

func (f *fakeDocker) CheckVersion(context.Context) (string, bool, error) {
	return f.version, f.reached, f.versionErr
}
func (f *fakeDocker) Containers(context.Context) ([]docker.Container, error) { return f.cs, f.listErr }
func (f *fakeDocker) Container(_ context.Context, id string) (docker.ContainerJSON, error) {
	f.inspected++
	if err := f.inspectErr[id]; err != nil {
		return docker.ContainerJSON{}, err
	}
	return f.inspects[id], nil
}
func (f *fakeDocker) Stats(_ context.Context, id string) (docker.Stats, error) {
	if err := f.statsErr[id]; err != nil {
		return docker.Stats{}, err
	}
	return f.stats[id], nil
}

var notFound = &docker.APIError{Status: 404, Message: "gone"}

func ct(id, name, state string, labels map[string]string, nets map[string]docker.Endpoint) docker.Container {
	c := docker.Container{ID: id, Names: []string{"/" + name}, ImageID: "sha256:" + id, State: state, Labels: labels}
	c.HostConfig.NetworkMode = "bridge"
	c.NetworkSettings.Networks = nets
	return c
}

func inspect(pid int, image string) docker.ContainerJSON {
	var j docker.ContainerJSON
	j.State.Pid, j.Config.Image = pid, image
	return j
}

func mustParse(t *testing.T, text string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(text), func(string) (string, bool) { return "", false }, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func text(fams []metrics.Family) string {
	var b bytes.Buffer
	metrics.Write(&b, fams)
	return b.String()
}

func newCore(t *testing.T, cfg *config.Config, f *fakeDocker, windows bool) (*Core, *bytes.Buffer) {
	var log bytes.Buffer
	c := New(cfg, Options{Docker: f, RootFS: t.TempDir(), Windows: windows, Log: slog.New(slog.NewTextHandler(&log, nil))})
	return c, &log
}

func TestReconcileErrors(t *testing.T) {
	cfg := mustParse(t, "")
	f := &fakeDocker{reached: true, version: "1.40", versionErr: errors.New("old")}
	c, _ := newCore(t, cfg, f, false)
	if err := c.Reconcile(context.Background()); !errors.Is(err, ErrDockerTooOld) {
		t.Fatalf("too old: %v", err)
	}
	f.reached = false
	if err := c.Reconcile(context.Background()); err != nil || c.Ready() {
		t.Fatalf("unreached: %v", err)
	}
	f.versionErr, f.version = nil, "1.47"
	f.listErr = errors.New("list")
	c.Reconcile(context.Background())
	f.listErr = nil
	f.cs = []docker.Container{ct("a", "a", "running", nil, nil)}
	f.inspectErr = map[string]error{"a": errors.New("inspect")}
	c.Reconcile(context.Background())
	if _, e := c.Stats(); e != 3 || c.Ready() {
		t.Fatalf("errors %v", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Reconcile(ctx)
	if _, e := c.Stats(); e != 3 {
		t.Fatal("canceled reconcile counted")
	}
	f.inspectErr = map[string]error{"a": notFound}
	if err := c.Reconcile(context.Background()); err != nil || !c.Ready() || len(c.targets) != 0 {
		t.Fatalf("404: %v %v", err, c.targets)
	}
	if c.Config() != cfg {
		t.Fatal("config")
	}
}

func TestTargets(t *testing.T) {
	cfg := mustParse(t, `
exclude: { containers: [ "skip*" ], images: [ "busybox*" ] }
container_labels: [ team ]
labels: [ { match: { stack: app }, labels: { tier: web } } ]
`)
	nets := map[string]docker.Endpoint{"z": {IPAddress: "10.0.0.2", IPPrefixLen: 24}, "a": {EndpointID: "e"}}
	f := &fakeDocker{version: "1.47", reached: true, cs: []docker.Container{
		ct("1", "web", "running", map[string]string{"com.docker.compose.project": "app", "team": "x",
			config.LabelSelf + "network": "bad"}, nets),
		ct("2", "stopped", "exited", nil, nil),
		ct("3", "skipme", "running", nil, nil),
		ct("4", "bb", "paused", nil, nil),
		ct("5", "off", "running", map[string]string{config.LabelEnable: "false"}, nil),
		ct("6", "hostnet", "restarting", map[string]string{"com.docker.stack.namespace": "st"}, nil),
		ct("7", "local", "running", nil, nil),
	}, inspects: map[string]docker.ContainerJSON{
		"1": inspect(0, "nginx:1"), "3": inspect(0, "nginx"), "4": inspect(0, "busybox:1"), "5": inspect(0, "x"),
		"6": inspect(0, "y"), "7": inspect(0, "sha256:7"),
	}}
	f.cs[5].HostConfig.NetworkMode = "host"
	c, log := newCore(t, cfg, f, false)
	c.Reconcile(context.Background())
	got := map[string]target{}
	for _, tg := range c.targets {
		got[tg.name] = tg
	}
	if len(got) != 3 || got["web"].stack != "app" || got["web"].labels["container_label_team"] != "x" ||
		got["web"].labels["tier"] != "web" || got["web"].labels["network"] != "" || got["hostnet"].stack != "st" ||
		!got["hostnet"].noNet || got["web"].noNet || got["web"].networks[0].name != "a" || got["local"].name != "local" {
		t.Fatalf("targets: %+v", got)
	}
	if !strings.Contains(log.String(), "label skipped") {
		t.Fatal("own label not warned")
	}
	// Cached inspects are kept for running containers only.
	f.cs = f.cs[:1]
	c.Reconcile(context.Background())
	if len(c.cache) != 1 {
		t.Fatalf("cache %v", c.cache)
	}
}

func TestWindows(t *testing.T) {
	cfg := mustParse(t, "")
	var st docker.Stats
	st.CPUStats.CPUUsage.TotalUsage = 25e6
	st.MemoryStats.Commit, st.MemoryStats.PrivateWorkingSet = 300, 200
	st.StorageStats.ReadBytes, st.StorageStats.WriteBytes, st.StorageStats.Reads, st.StorageStats.Writes = 1, 2, 3, 4
	st.Networks = map[string]docker.NetStats{"E1": {RxBytes: 10, TxBytes: 20}, "other": {RxBytes: 1}}
	nets := map[string]docker.Endpoint{"nat": {EndpointID: "e1"}}
	two := map[string]docker.Endpoint{"nat": {}, "x": {}}
	j := inspect(0, "nanoserver")
	j.HostConfig.NanoCpus, j.HostConfig.Memory = 15e8, 1<<20
	f := &fakeDocker{version: "1.47", reached: true,
		cs: []docker.Container{ct("1", "w", "running", nil, nets), ct("2", "gone", "running", nil, nil),
			ct("3", "failing", "running", nil, nil), ct("4", "nonet", "running", nil, nil),
			ct("5", "multi", "running", nil, two), ct("6", "quiet", "running", nil, nets)},
		inspects: map[string]docker.ContainerJSON{"1": j, "2": inspect(0, "a"), "3": inspect(0, "a"), "4": inspect(0, "a"),
			"5": inspect(0, "a"), "6": inspect(0, "a")},
		stats:    map[string]docker.Stats{"1": st, "4": st, "5": st, "6": {}},
		statsErr: map[string]error{"2": notFound, "3": errors.New("busy")},
	}
	f.cs[3].HostConfig.NetworkMode = "none"
	c, _ := newCore(t, cfg, f, true)
	c.Reconcile(context.Background())
	c.Reconcile(context.Background())
	if f.inspected != 6 {
		t.Fatalf("Windows inspects are cached: %d", f.inspected)
	}
	out := text(c.Collect())
	for _, want := range []string{
		`hatchlens_container_cpu_usage_seconds_total{container="w"} 2.5`,
		`hatchlens_container_cpu_limit_cores{container="w"} 1.5`,
		`hatchlens_container_memory_usage_bytes{container="w"} 300`,
		`hatchlens_container_memory_working_set_bytes{container="w"} 200`,
		`hatchlens_container_memory_limit_bytes{container="w"} 1.048576e+06`,
		`hatchlens_container_disk_writes_total{container="w"} 4`,
		`hatchlens_container_network_receive_bytes_total{container="w"} 11`,
		`hatchlens_container_network_transmit_bytes_total{container="w"} 20`,
		`hatchlens_container_network_receive_bytes_total{container="multi"} 11`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, `container="failing"`) || strings.Contains(out, `container="gone"`) ||
		strings.Contains(out, `throttled`) || strings.Contains(out, `oom`) ||
		strings.Contains(out, `network_receive_bytes_total{container="nonet"`) ||
		strings.Contains(out, `network_receive_bytes_total{container="quiet"`) || strings.Contains(out, `interface=`) {
		t.Fatalf("unexpected series:\n%s", out)
	}
}

func TestWindowsNetNames(t *testing.T) {
	cfg := mustParse(t, "")
	var st docker.Stats
	st.Networks = map[string]docker.NetStats{"e1": {RxBytes: 10}, "E2": {RxBytes: 5}, "other": {RxBytes: 1}}
	f := &fakeDocker{version: "1.47", reached: true,
		cs:       []docker.Container{ct("1", "w", "running", nil, nil)},
		inspects: map[string]docker.ContainerJSON{"1": inspect(0, "a")},
		stats:    map[string]docker.Stats{"1": st},
	}
	names := func(context.Context) (map[string]string, error) {
		return map[string]string{"E1": "nat", "E2": "netA"}, nil
	}
	c := New(cfg, Options{Docker: f, Windows: true, NetNames: names, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	c.Reconcile(context.Background())
	out := text(c.Collect())
	for _, want := range []string{
		`hatchlens_container_network_receive_bytes_total{container="w",network="nat"} 10`,
		`hatchlens_container_network_receive_bytes_total{container="w",network="netA"} 5`,
		`hatchlens_container_network_receive_bytes_total{container="w"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	broken := func(context.Context) (map[string]string, error) { return nil, errors.New("no service") }
	c = New(cfg, Options{Docker: f, Windows: true, NetNames: broken, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	c.Reconcile(context.Background())
	if out := text(c.Collect()); !strings.Contains(out, `hatchlens_container_network_receive_bytes_total{container="w"} 16`) {
		t.Fatalf("without names one sum:\n%s", out)
	}
}

// fakeHost writes the cgroup and /proc files of one container.
func fakeHost(t *testing.T, root, id string, pid, cgPath string) {
	write(t, filepath.Join(root, "proc", pid, "cgroup"), "0::"+cgPath+"\n")
	dir := filepath.Join(root, "sys", "fs", "cgroup", "system.slice", "docker-"+id+".scope")
	write(t, filepath.Join(root, "sys", "fs", "cgroup", "cgroup.controllers"), "cpu memory io\n")
	write(t, filepath.Join(dir, "cpu.stat"), "usage_usec 2500000\nuser_usec 1\nthrottled_usec 500000\nbad\n")
	write(t, filepath.Join(dir, "cpu.max"), "50000 100000\n")
	write(t, filepath.Join(dir, "memory.current"), "1000\n")
	write(t, filepath.Join(dir, "memory.stat"), "anon 600\ninactive_file 300\nx y\n")
	write(t, filepath.Join(dir, "memory.max"), "max\n")
	write(t, filepath.Join(dir, "memory.events"), "low 0\noom_kill 2\n")
	write(t, filepath.Join(dir, "io.stat"), "8:0 rbytes=10 wbytes=20 rios=1 wios=2 dbytes=0 dios=0\n8:16 rbytes=5 wbytes=5 rios=1 wios=1 junk\n")
	net := filepath.Join(root, "proc", pid, "net")
	write(t, filepath.Join(net, "dev"), `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9 9 0 0 0 0 0 0 9 9 0 0 0 0 0 0
  eth0: 100 10 1 2 0 0 0 0 200 20 3 4 0 0 0 0
  eth1: 300 30 0 0 0 0 0 0 400 40 0 0 0 0 0 0
  eth2: 500 50 0 0 0 0 0 0 600 60 0 0 0 0 0 0
 short: 1 2
`)
	write(t, filepath.Join(net, "route"), "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"eth0\t00000000\t010012AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"+
		"eth0\t000012AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"+
		"eth1\t000013AC\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"+
		"bad\n")
	write(t, filepath.Join(net, "if_inet6"), "fd000000000000000000000000000002 0b 40 00 80 eth2\nshort\n")
}

func TestLinux(t *testing.T) {
	cfg := mustParse(t, "")
	nets := map[string]docker.Endpoint{
		"front": {IPAddress: "172.18.0.5", IPPrefixLen: 16}, "back": {IPAddress: "172.19.0.0", IPPrefixLen: 24},
		"v6": {GlobalIPv6Address: "fd00::2"}, "missing": {IPAddress: "10.9.9.9", IPPrefixLen: 8},
	}
	f := &fakeDocker{version: "1.47", reached: true,
		cs:       []docker.Container{ct("abc", "web", "running", map[string]string{"com.docker.compose.project": "app"}, nets)},
		inspects: map[string]docker.ContainerJSON{"abc": inspect(42, "nginx")}}
	c, _ := newCore(t, cfg, f, false)
	fakeHost(t, c.root, "abc", "42", "/system.slice/docker-abc.scope")
	c.Reconcile(context.Background())
	c.Reconcile(context.Background())
	if f.inspected != 1 {
		t.Fatalf("owned PID must stay cached: %d", f.inspected)
	}
	out := text(c.Collect())
	for _, want := range []string{
		`hatchlens_container_cpu_usage_seconds_total{container="web",stack="app"} 2.5`,
		`hatchlens_container_cpu_throttled_seconds_total{container="web",stack="app"} 0.5`,
		`hatchlens_container_cpu_limit_cores{container="web",stack="app"} 0.5`,
		`hatchlens_container_memory_usage_bytes{container="web",stack="app"} 1000`,
		`hatchlens_container_memory_working_set_bytes{container="web",stack="app"} 700`,
		`hatchlens_container_memory_oom_kills_total{container="web",stack="app"} 2`,
		`hatchlens_container_disk_read_bytes_total{container="web",stack="app"} 15`,
		`hatchlens_container_disk_written_bytes_total{container="web",stack="app"} 25`,
		`hatchlens_container_disk_reads_total{container="web",stack="app"} 2`,
		`hatchlens_container_disk_writes_total{container="web",stack="app"} 3`,
		`hatchlens_container_network_receive_bytes_total{container="web",interface="eth0",network="front",stack="app"} 100`,
		`hatchlens_container_network_receive_errors_total{container="web",interface="eth0",network="front",stack="app"} 1`,
		`hatchlens_container_network_transmit_dropped_total{container="web",interface="eth0",network="front",stack="app"} 4`,
		`hatchlens_container_network_transmit_bytes_total{container="web",interface="eth1",network="back",stack="app"} 400`,
		`hatchlens_container_network_receive_packets_total{container="web",interface="eth2",network="v6",stack="app"} 50`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, `interface="lo"`) || strings.Contains(out, `interface="short"`) ||
		strings.Contains(out, "memory_limit_bytes") {
		t.Fatalf("unexpected:\n%s", out)
	}

	// Disabled functions are not reported.
	c.cfg = mustParse(t, "disable: [ cpu, ram, disk ]")
	if out := text(c.Collect()); strings.Contains(out, "cpu_") || strings.Contains(out, "memory_") ||
		strings.Contains(out, "disk_") || !strings.Contains(out, "network_") {
		t.Fatalf("disable:\n%s", out)
	}
	c.cfg = cfg

	// Limits, no memory.stat, no memory.events, no io.stat.
	dir := filepath.Join(c.root, "sys", "fs", "cgroup", "system.slice", "docker-abc.scope")
	write(t, filepath.Join(dir, "cpu.max"), "max 100000\n")
	write(t, filepath.Join(dir, "memory.max"), "2048\n")
	for _, n := range []string{"memory.stat", "memory.events", "io.stat", "cpu.stat"} {
		os.Remove(filepath.Join(dir, n))
	}
	write(t, filepath.Join(dir, "cpu.stat"), "usage_usec 1000000\n")
	out = text(c.Collect())
	if !strings.Contains(out, `memory_working_set_bytes{container="web",stack="app"} 1000`) ||
		!strings.Contains(out, `memory_limit_bytes{container="web",stack="app"} 2048`) ||
		strings.Contains(out, "cpu_limit_cores") || strings.Contains(out, "oom") || strings.Contains(out, "disk_") ||
		strings.Contains(out, "throttled") {
		t.Fatalf("limits:\n%s", out)
	}
	os.Remove(filepath.Join(dir, "memory.current"))
	if strings.Contains(text(c.Collect()), "memory_") {
		t.Fatal("memory without memory.current")
	}

	// PID reused by another process: no series, inspected again at the next reconcile.
	write(t, filepath.Join(c.root, "proc", "42", "cgroup"), "0::/user.slice\n")
	if out := text(c.Collect()); strings.Contains(out, "web") {
		t.Fatalf("foreign PID:\n%s", out)
	}
	f.inspects["abc"] = inspect(0, "nginx")
	c.Reconcile(context.Background())
	if f.inspected != 2 || c.targets[0].pid != 0 || text(c.Collect()) != "" {
		t.Fatalf("restart: %d %+v", f.inspected, c.targets)
	}
}

func TestCgroupV1AndNoNet(t *testing.T) {
	cfg := mustParse(t, "")
	f := &fakeDocker{version: "1.47", reached: true,
		cs:       []docker.Container{ct("abc", "web", "running", nil, nil)},
		inspects: map[string]docker.ContainerJSON{"abc": inspect(42, "nginx")}}
	f.cs[0].HostConfig.NetworkMode = "container:other"
	c, log := newCore(t, cfg, f, false)
	fakeHost(t, c.root, "abc", "42", "/system.slice/docker-abc.scope")
	os.Remove(filepath.Join(c.root, "sys", "fs", "cgroup", "cgroup.controllers"))
	c.Reconcile(context.Background())
	if out := text(c.Collect()); out != "" {
		t.Fatalf("v1 and shared netns:\n%s", out)
	}
	c.Collect()
	if strings.Count(log.String(), "no cgroup v2") != 1 {
		t.Fatalf("log once:\n%s", log)
	}
	// No /proc/<pid>/net/dev: no network series.
	f.cs[0].HostConfig.NetworkMode = "bridge"
	c.Reconcile(context.Background())
	os.Remove(filepath.Join(c.root, "proc", "42", "net", "dev"))
	if out := text(c.Collect()); out != "" {
		t.Fatalf("no dev:\n%s", out)
	}
}

func TestRelativeCgroup(t *testing.T) {
	cfg := mustParse(t, "")
	f := &fakeDocker{version: "1.47", reached: true,
		cs:       []docker.Container{ct("abc", "web", "running", nil, nil)},
		inspects: map[string]docker.ContainerJSON{"abc": inspect(42, "nginx")}}
	c, log := newCore(t, cfg, f, false)
	fakeHost(t, c.root, "abc", "42", "/../docker-abc.scope")
	cg := filepath.Join(c.root, "sys", "fs", "cgroup")
	write(t, filepath.Join(cg, "cgroup.procs"), "1\n")
	write(t, filepath.Join(cg, "system.slice", "docker-self.scope", "cgroup.procs"), "7\n77\n")
	write(t, filepath.Join(cg, "a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "cgroup.procs"), "77\n")
	write(t, filepath.Join(c.root, "proc", "self", "cgroup"), "0::/\n")
	c.selfPID = func() int { return 77 }
	c.Reconcile(context.Background())
	if out := text(c.Collect()); !strings.Contains(out, `cpu_usage_seconds_total{container="web"} 2.5`) {
		t.Fatalf("relative path:\n%s\n%s", out, log)
	}
	if b, err := c.ownBase(); b != "/system.slice/docker-self.scope" || err != nil {
		t.Fatalf("base %q %v", b, err)
	}

	// Own process in a sub-cgroup of its namespace.
	c2, _ := newCore(t, cfg, f, false)
	cg2 := filepath.Join(c2.root, "sys", "fs", "cgroup")
	write(t, filepath.Join(cg2, "kubepods", "pod", "inner", "cgroup.procs"), "77\n")
	write(t, filepath.Join(c2.root, "proc", "self", "cgroup"), "1:x:/\n0::/inner\n")
	c2.selfPID = func() int { return 77 }
	if b, err := c2.ownBase(); b != "/kubepods/pod" || err != nil {
		t.Fatalf("nested base %q %v", b, err)
	}

	// Own process in the root cgroup (host namespace).
	c3, _ := newCore(t, cfg, f, false)
	write(t, filepath.Join(c3.root, "sys", "fs", "cgroup", "cgroup.procs"), "77\n")
	c3.selfPID = func() int { return 77 }
	if b, err := c3.ownBase(); b != "/" || err != nil {
		t.Fatalf("root base %q %v", b, err)
	}

	// Own cgroup not found: logged once per container, no CPU series.
	c4, log4 := newCore(t, cfg, f, false)
	fakeHost(t, c4.root, "abc", "42", "/../docker-abc.scope")
	c4.selfPID = func() int { return 99 }
	c4.Reconcile(context.Background())
	c4.Collect()
	if out := text(c4.Collect()); strings.Contains(out, "cpu_") || strings.Count(log4.String(), "container cgroup not found") != 1 {
		t.Fatalf("no base:\n%s\n%s", out, log4)
	}
	c5, _ := newCore(t, cfg, f, false)
	c5.selfPID = func() int { return 77 }
	if _, err := c5.ownBase(); err == nil {
		t.Fatal("no cgroup tree")
	}
}

func TestParsers(t *testing.T) {
	if routeIface(nil, "x", 8) != "" || routeIface(nil, "fd00::1", 64) != "" || addrIface(nil, "1.2.3.4") != "" || addrIface(nil, "fd00::9") != "" ||
		addrIface(nil, "x") != "" || hexAddr("zz") != [4]byte{} || maskBits([4]byte{255, 255, 240, 0}) != 20 {
		t.Fatal("parsers")
	}
	if limit([]byte("max")) != 0 || limit([]byte("12\n")) != 12 {
		t.Fatal("limit")
	}
	c := New(mustParse(t, ""), Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if c.owns(1, "x") || c.cgroupOf(1) != "" {
		t.Fatal("no proc")
	}
	c.root = t.TempDir()
	write(t, filepath.Join(c.root, "proc", "43", "cgroup"), "1:name=systemd:/docker/x\n")
	if c.cgroupOf(43) != "" || !c.owns(43, "x") || c.owns(43, "y") {
		t.Fatal("cgroup v1 line")
	}
}

func TestNewDefaults(t *testing.T) {
	c := New(mustParse(t, ""), Options{})
	if c.log == nil || c.now == nil || c.selfPID == nil {
		t.Fatal("defaults")
	}
}
