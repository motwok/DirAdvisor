// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/metrics"
)

// Scrapes within usageTTL share one quota read; parallel scrapes wait for the
// read in flight instead of starting their own (spec MEA-3).
func TestUsageCacheAndSingleflight(t *testing.T) {
	e := newEnv(t)
	e.volume("v", nil, nil)
	e.reconcile() // allocate reads the quotas once, not counted below
	calls := func(do func()) int {
		before := e.fs.usageCalls
		do()
		return e.fs.usageCalls - before
	}
	scrape := func() { e.metricsText() }
	if n := calls(func() { scrape(); scrape() }); n != 1 {
		t.Fatalf("calls within ttl: %d", n)
	}
	e.now = e.now.Add(usageTTL)
	if n := calls(scrape); n != 1 {
		t.Fatalf("calls after ttl: %d", n)
	}
	// A filesystem not in the cache forces a read.
	e.fs.mkdir("/srv/x")
	e.fs.mounts["/srv"] = hostMount("/srv", "xfs", "/dev/sdb")
	e.cfg.Directories = []config.Directory{{Path: "/srv/x"}}
	e.reconcile()
	if n := calls(scrape); n != 2 {
		t.Fatalf("calls with new group: %d", n)
	}

	e.now = e.now.Add(usageTTL)
	before := e.fs.usageCalls
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.fs.onUsage = func() {
		once.Do(func() { close(started) })
		<-release
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.c.Collect(context.Background()) }()
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if e.fs.usageCalls != before+2 {
		t.Fatalf("parallel scrapes: %d calls", e.fs.usageCalls-before)
	}
	// Waiters needing a group the read in flight lacks share the one read
	// started after it instead of each starting their own.
	e.now = e.now.Add(usageTTL)
	gate := make(chan struct{})
	var skip atomic.Bool
	e.fs.onUsage = func() {
		if !skip.Load() {
			<-gate
		}
	}
	first := make(chan struct{})
	go func() { e.c.Collect(context.Background()); close(first) }()
	time.Sleep(20 * time.Millisecond)
	skip.Store(true)
	e.fs.mkdir("/srv/y")
	e.fs.mounts["/srv/y"] = hostMount("/srv/y", "xfs", "/dev/sdc")
	e.cfg.Directories = append(e.cfg.Directories, config.Directory{Path: "/srv/y"})
	e.reconcile()
	skip.Store(false)
	before = e.fs.usageCalls
	var waiters sync.WaitGroup
	for i := 0; i < 3; i++ {
		waiters.Add(1)
		go func() { defer waiters.Done(); e.c.Collect(context.Background()) }()
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	<-first
	waiters.Wait()
	// The first read's second mount, then one read of the three mounts (not
	// one per waiter).
	if n := e.fs.usageCalls - before; n != 4 {
		t.Fatalf("second-level waiters: %d calls", n)
	}
	e.fs.onUsage = func() {
		once.Do(func() { close(started) })
		<-release
	}
	// A waiter whose context ends while the read hangs gives up without usage.
	e.now = e.now.Add(usageTTL)
	started, release = make(chan struct{}), make(chan struct{})
	once = sync.Once{}
	go e.c.Collect(context.Background())
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	bytesSamples := func(fams []metrics.Family) int {
		for _, f := range fams {
			if f.Name == "hatchlens_directory_bytes" {
				return len(f.Samples)
			}
		}
		return 0
	}
	if n := bytesSamples(e.c.Collect(ctx)); n != 0 {
		t.Fatalf("usage from a cancelled wait: %d samples", n)
	}
	close(release)
	if n := bytesSamples(e.c.Collect(context.Background())); n == 0 {
		t.Fatal("usage after the read")
	}
}

// The ZFS feature check is per pool, not per dataset.
func TestZFSStatusPerPool(t *testing.T) {
	e := newEnv(t)
	e.fs.mkdir("/tank/a")
	e.fs.mkdir("/tank/b")
	e.fs.mounts["/tank/a"] = hostMount("/tank/a", "zfs", "tank/a")
	e.fs.mounts["/tank/b"] = hostMount("/tank/b", "zfs", "tank/b")
	e.fs.status["/tank/b"] = "never asked"
	e.cfg.Directories = []config.Directory{{Path: "/tank/a"}, {Path: "/tank/b"}}
	e.reconcile()
	if e.entry("/tank/b") == nil || e.reason("/tank/b") != "" {
		t.Fatalf("second dataset asked the pool again: %+v", e.c.view.unmeas)
	}
}

// The handlers do not wait for a reconcile that holds the state lock.
func TestCollectAndReadyWhileReconcileHoldsLock(t *testing.T) {
	e := newEnv(t)
	e.volume("v", nil, nil)
	e.reconcile()
	e.c.mu.Lock()
	defer e.c.mu.Unlock()
	done := make(chan bool, 1)
	go func() {
		e.c.Collect(context.Background())
		done <- e.c.Ready()
	}()
	select {
	case ready := <-done:
		if !ready {
			t.Fatal("not ready")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Collect blocked on the state lock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if e.c.SaveWithin(ctx) {
		t.Fatal("save must give up while the lock is held")
	}
}

func TestSaveWithinSaves(t *testing.T) {
	e := newEnv(t)
	e.volume("v", nil, nil)
	e.reconcile()
	e.c.st.Entries["x"] = nil
	delete(e.c.st.Entries, "x")
	e.c.st.DockerRoot = "/changed"
	if !e.c.SaveWithin(context.Background()) {
		t.Fatal("save")
	}
	e.start()
	if e.c.st.DockerRoot != "/changed" {
		t.Fatalf("not saved: %q", e.c.st.DockerRoot)
	}
}

// A reconcile cancelled during the Docker calls is not a Docker failure.
func TestReconcileCancelled(t *testing.T) {
	e := newEnv(t)
	e.volume("v", nil, nil)
	e.reconcile()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.dk.down = true
	if err := e.c.Reconcile(ctx); err != nil || e.c.recErrors != 0 || len(e.c.view.entries) != 1 {
		t.Fatalf("err %v errors %v entries %d", err, e.c.recErrors, len(e.c.view.entries))
	}
}
