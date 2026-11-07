// SPDX-License-Identifier: AGPL-3.0-or-later

package images

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/images/state"
)

const nginx = "docker.io/library/nginx:1.29"

func TestUpdateFlow(t *testing.T) {
	r := newRig(t, "images: { check_interval: 1h }")
	r.f.add("web", "nginx:1.29", "sha256:old", "running", map[string]string{"com.docker.compose.project": "s"},
		"nginx@sha256:idx1")
	r.reconcile()
	if !r.c.Ready() {
		t.Fatal("not ready")
	}
	r.has(`hatchlens_container_image_info{container="web",mode="pending",reference="`+nginx+`",state="running"} 1`,
		`hatchlens_container_image_update_pulled{container="web",image="nginx:1.29",stack="s"} 0`)
	r.hasNot("update_available{", "image_check_success")

	// First check: same digest, up to date; next due at a random point in the interval.
	r.f.dist[nginx] = "sha256:idx1"
	r.checkAll()
	r.reconcile()
	r.has(`hatchlens_container_image_update_available{container="web",image="nginx:1.29",stack="s"} 0`,
		`hatchlens_image_check_success{reference="`+nginx+`"} 1`,
		`hatchlens_image_check_timestamp_seconds{reference="`+nginx+`"} 1e+06`)
	if rs := r.c.st.Refs[nginx]; rs.Due != 1_000_000+1800 {
		t.Fatalf("first spread: %d", rs.Due)
	}

	// A newer image in the registry.
	r.f.dist[nginx] = "sha256:idx2"
	r.clk.add(31 * time.Minute)
	r.checkAll()
	if rs := r.c.st.Refs[nginx]; rs.Due != r.clk.now().Unix()+3600 {
		t.Fatalf("regular interval: %d", rs.Due)
	}
	r.reconcile()
	since := r.clk.now().Unix()
	r.has(`hatchlens_container_image_update_available{container="web",image="nginx:1.29",stack="s"} 1`,
		`hatchlens_container_image_update_available_since_timestamp_seconds{container="web",image="nginx:1.29",stack="s"} 1.00186e+06`)
	if r.c.st.Since[state.SinceKey("web", nginx)].Since != since {
		t.Fatal("since not stored")
	}

	// The registry moves on again: since stays.
	r.f.dist[nginx] = "sha256:idx3"
	r.clk.add(time.Hour)
	r.checkAll()
	r.reconcile()
	if r.c.st.Since[state.SinceKey("web", nginx)].Since != since {
		t.Fatal("since changed")
	}

	// Restart keeps since and the digest, and checks nothing at start.
	r.c.Save()
	calls := len(r.f.distRefs)
	r.start()
	r.reconcile()
	r.c.CheckDue(context.Background())
	if len(r.f.distRefs) != calls {
		t.Fatal("check at start for a known reference")
	}
	r.has(`hatchlens_container_image_update_available_since_timestamp_seconds{container="web",image="nginx:1.29",stack="s"} 1.00186e+06`)

	// Pulled, not recreated.
	r.f.images[nginx] = docker.Image{ID: "sha256:new", RepoDigests: []string{"nginx@sha256:idx3"}}
	r.reconcile()
	r.has(`hatchlens_container_image_update_pulled{container="web",image="nginx:1.29",stack="s"} 1`)
	if !strings.Contains(r.log.String(), "container image changed") {
		t.Fatal("change not logged")
	}

	// Recreated on the new image (containerd store: the image ID is the index digest).
	r.f.containers[0].ImageID = "sha256:idx3"
	r.f.images["sha256:idx3"] = docker.Image{ID: "sha256:idx3"}
	r.f.images[nginx] = docker.Image{ID: "sha256:idx3"}
	r.reconcile()
	r.has(`hatchlens_container_image_update_available{container="web",image="nginx:1.29",stack="s"} 0`,
		`hatchlens_container_image_update_pulled{container="web",image="nginx:1.29",stack="s"} 0`)
	r.hasNot("since_timestamp_seconds{")
}

func TestModes(t *testing.T) {
	r := newRig(t, `
exclude:
  containers: [ "skip-*" ]
  images: [ "ghcr.io/hidden/*", "sha256:dead*" ]
`)
	id := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	r.f.add("pinned", "nginx@sha256:pin", "sha256:p", "exited", nil)
	r.f.add("byid", "0123456789ab", id, "running", nil)
	r.f.add("byid-hidden", "sha256:deadbeef", "sha256:deadbeef00", "running", nil)
	r.f.add("skip-me", "nginx", "sha256:s", "running", nil)
	r.f.add("hidden", "ghcr.io/hidden/app:1", "sha256:h", "running", nil)
	r.f.add("off", "nginx", "sha256:o", "running", map[string]string{labelEnable: "false"})
	r.f.add("gone", "nginx", "sha256:g", "removing", nil)
	r.f.add("paused", "nginx", "sha256:x", "paused", nil)
	r.f.add("created", "nginx", "sha256:x", "created", nil)
	r.reconcile()
	r.has(`hatchlens_container_image_info{container="pinned",mode="pinned",reference="docker.io/library/nginx@sha256:pin",state="stopped"} 1`,
		`hatchlens_container_image_update_pulled{container="pinned",image="nginx@sha256:pin"} 0`,
		`hatchlens_container_image_info{container="byid",mode="local",state="running"} 1`,
		`hatchlens_container_image_info{container="paused",mode="pending",reference="docker.io/library/nginx:latest",state="running"} 1`,
		`hatchlens_container_image_info{container="created",mode="pending",reference="docker.io/library/nginx:latest",state="stopped"} 1`)
	r.hasNot(`container="skip-me"`, `container="hidden"`, `container="off"`, `container="gone"`, `container="byid-hidden"`,
		`update_pulled{container="byid"`)
	r.checkAll()
	for _, ref := range r.f.distRefs {
		if strings.Contains(ref, "hidden") || strings.Contains(ref, "pin") {
			t.Fatalf("checked %s", ref)
		}
	}
}

func TestSwarm(t *testing.T) {
	r := newRig(t, "")
	task := func(id, state string) map[string]string {
		return map[string]string{swarmTask: id, "com.docker.swarm.service.name": "st_web",
			"com.docker.swarm.task.name": "st_web.2." + id, "com.docker.stack.namespace": "st"}
	}
	r.f.add("st_web.2.aaa", "reg:5000/sw:1@sha256:old", "sha256:old", "running", task("aaa", "running"))
	r.f.add("st_web.2.hist", "reg:5000/sw:1@sha256:older", "sha256:older", "exited", task("hist", "exited"))
	r.f.add("st_pin.1.bbb", "reg:5000/pin@sha256:x", "sha256:x", "running",
		map[string]string{swarmTask: "bbb", "com.docker.swarm.service.name": "st_pin", "com.docker.swarm.task.name": "st_pin.1.bbb"})
	r.f.dist["reg:5000/sw:1"] = "sha256:new"
	r.reconcile()
	r.checkAll()
	r.reconcile()
	r.has(`hatchlens_container_image_update_available{container="st_web.2.aaa",image="reg:5000/sw:1",stack="st"} 1`,
		`hatchlens_container_image_info{container="st_pin.1.bbb",mode="pinned",reference="reg:5000/pin@sha256:x",state="running"} 1`)
	r.hasNot(`st_web.2.hist`, `update_pulled{container="st_web`, `update_pulled{container="st_pin`)
	since := r.c.st.Since["swarm:st_web.2|reg:5000/sw:1"]
	if since == nil {
		t.Fatalf("since key: %v", r.c.st.Since)
	}
	// service update --force: a new task on the old image keeps since.
	r.clk.add(time.Minute)
	r.f.containers[0] = docker.Container{ID: "c-new", Names: []string{"/st_web.2.ccc"}, ImageID: "sha256:old",
		State: "running", Labels: task("ccc", "running")}
	r.f.configs["c-new"] = "reg:5000/sw:1@sha256:old"
	r.reconcile()
	if r.c.st.Since["swarm:st_web.2|reg:5000/sw:1"].Since != since.Since {
		t.Fatal("since lost across tasks")
	}
	if _, ok := r.c.cfgImages["c-st_web.2.aaa"]; ok {
		t.Fatal("cache of a gone container kept")
	}
}

// start-first: the new task on the current image and the old one share a slot
// for a while; since stays until the old task is gone.
func TestSwarmStartFirst(t *testing.T) {
	r := newRig(t, "")
	task := func(id string) map[string]string {
		return map[string]string{swarmTask: id, "com.docker.swarm.service.name": "st_web",
			"com.docker.swarm.task.name": "st_web.1." + id}
	}
	r.f.add("st_web.1.old", "reg:5000/sw:1@sha256:old", "sha256:old", "running", task("old"))
	r.f.dist["reg:5000/sw:1"] = "sha256:new"
	r.reconcile()
	r.checkAll()
	r.reconcile()
	since := r.c.st.Since["swarm:st_web.1|reg:5000/sw:1"].Since
	r.clk.add(time.Hour)
	// Docker lists the newest container first.
	r.f.containers = append([]docker.Container{{ID: "c-new", Names: []string{"/st_web.1.new"}, ImageID: "sha256:new",
		State: "running", Labels: task("new")}}, r.f.containers...)
	r.f.configs["c-new"] = "reg:5000/sw:1@sha256:new"
	r.f.images["sha256:new"] = docker.Image{ID: "sha256:new"}
	r.reconcile()
	r.has(`hatchlens_container_image_update_available{container="st_web.1.new",image="reg:5000/sw:1"} 0`,
		`hatchlens_container_image_update_available{container="st_web.1.old",image="reg:5000/sw:1"} 1`,
		`hatchlens_container_image_update_available_since_timestamp_seconds{container="st_web.1.old",image="reg:5000/sw:1"} 1e+06`)
	if s := r.c.st.Since["swarm:st_web.1|reg:5000/sw:1"]; s == nil || s.Since != since {
		t.Fatalf("since replaced: %+v", s)
	}
	r.f.containers = r.f.containers[:1]
	r.reconcile()
	if r.c.st.Since["swarm:st_web.1|reg:5000/sw:1"] != nil {
		t.Fatal("since kept after the old task left")
	}
}

func TestLabels(t *testing.T) {
	r := newRig(t, `
container_labels:
 - com.example.team
 - { from: com.example.owner, to: owner_team }
labels:
 - match: { stack: "mail*" }
   labels: { team: mail, tier: a }
 - match: { container: "web", image: "docker.io/library/nginx:*" }
   labels: { tier: b }
 - match: { image: "nothing" }
   labels: { tier: c }
 - match: { volume: "*" }
   labels: { tier: d }
 - match: { path: "/*" }
   labels: { tier: e }
`)
	r.f.add("web", "nginx:1", "sha256:a", "running", map[string]string{
		"com.docker.compose.project": "mailcow", "com.example.team": "x", "com.example.owner": "o",
		labelSelf + "team": "self", labelSelf + "env": "prod", labelSelf + "mode": "bad", labelSelf + "1x": "bad"})
	r.f.add("id", "0123", "sha256:0123", "running", map[string]string{"com.example.team": "y"})
	r.f.dist["docker.io/library/nginx:1"] = "sha256:a"
	r.reconcile()
	r.checkAll()
	r.reconcile()
	r.reconcile()
	r.has(`hatchlens_container_image_update_available{container="web",container_label_com_example_team="x",env="prod",image="nginx:1",owner_team="o",stack="mailcow",team="mail",tier="b"} 0`)
	if n := strings.Count(r.log.String(), "label skipped"); n != 2 {
		t.Fatalf("warnings: %d\n%s", n, r.log.String())
	}
}

func TestCheckFailures(t *testing.T) {
	r := newRig(t, `
images:
 check_interval: 1h
 registries:
  - { host: ghcr.io, username: u, password: p }
  - { host: docker.io, username: hub, password: h }
  - { host: quay.io, username: q }
`)
	r.f.add("a", "ghcr.io/x/a:1", "sha256:a", "running", nil)
	r.f.add("b", "ghcr.io/x/b:1", "sha256:b", "running", nil)
	r.f.add("c", "nginx:1", "sha256:c", "running", nil)
	r.f.add("d", "quay.io/x/d:1", "sha256:d", "running", nil)
	r.f.distErr["ghcr.io/x/a:1"] = &docker.APIError{Status: 429, Message: "toomanyrequests"}
	r.f.distErr["docker.io/library/nginx:1"] = &docker.APIError{Status: 403, Message: "denied"}
	r.f.distErr["quay.io/x/d:1"] = errBoom
	r.reconcile()
	r.checkAll()
	r.reconcile()
	r.has(`hatchlens_image_check_error{reason="rate_limited",reference="ghcr.io/x/a:1"} 1`,
		`hatchlens_image_check_error{reason="denied",reference="docker.io/library/nginx:1"} 1`,
		`hatchlens_image_check_error{reason="unreachable",reference="quay.io/x/d:1"} 1`,
		`hatchlens_image_check_success{reference="ghcr.io/x/a:1"} 0`,
		`hatchlens_container_image_info{container="a",mode="error",reference="ghcr.io/x/a:1",state="running"} 1`,
		`hatchlens_container_image_info{container="b",mode="pending",reference="ghcr.io/x/b:1",state="running"} 1`)
	r.hasNot(`check_timestamp_seconds{reference="ghcr.io/x/a:1"}`)
	if !strings.Contains(r.log.String(), "registry check failed") || strings.Contains(r.log.String(), `password`) {
		t.Fatal(r.log.String())
	}
	// ghcr.io is backed off; b waits for it.
	if reg := r.c.st.Registries["ghcr.io"]; reg == nil || reg.Until != 1_000_000+300 || reg.Step != 1 {
		t.Fatalf("registry backoff: %+v", reg)
	}
	auths := map[string]string{}
	for i, ref := range r.f.distRefs {
		auths[ref] = r.f.auths[i]
	}
	if auths["quay.io/x/d:1"] != "" || auths["docker.io/library/nginx:1"] == "" {
		t.Fatalf("auths %v", auths)
	}
	var a map[string]string
	b, _ := base64.URLEncoding.DecodeString(auths["docker.io/library/nginx:1"])
	if json.Unmarshal(b, &a); a["serveraddress"] != "https://index.docker.io/v1/" || a["username"] != "hub" {
		t.Fatalf("hub auth %v", a)
	}
	b, _ = base64.URLEncoding.DecodeString(auths["ghcr.io/x/a:1"])
	if json.Unmarshal(b, &a); a["serveraddress"] != "ghcr.io" || a["password"] != "p" {
		t.Fatalf("ghcr auth %v", a)
	}
	// Backoff doubles; a success resets it.
	if rs := r.c.st.Refs["quay.io/x/d:1"]; rs.Due != 1_000_000+300 || rs.Fails != 1 {
		t.Fatalf("ref backoff: %+v", rs)
	}
	r.clk.add(5 * time.Minute)
	r.checkAll()
	if rs := r.c.st.Refs["quay.io/x/d:1"]; rs.Due != r.clk.now().Unix()+600 || rs.Fails != 2 {
		t.Fatalf("doubled: %+v", rs)
	}
	// b (older due) went first; a waits the 10 seconds between checks of one registry.
	if rs := r.c.st.Refs["ghcr.io/x/a:1"]; rs.Fails != 1 {
		t.Fatalf("a checked too early: %+v", rs)
	}
	r.clk.add(10 * time.Second)
	r.checkAll()
	if reg := r.c.st.Registries["ghcr.io"]; reg.Step != 2 || reg.Until != r.clk.now().Unix()+600 {
		t.Fatalf("registry doubled: %+v", reg)
	}
	delete(r.f.distErr, "ghcr.io/x/a:1")
	r.f.dist["ghcr.io/x/a:1"] = "sha256:a"
	r.clk.add(10 * time.Minute)
	r.checkAll()
	r.clk.add(10 * time.Second)
	r.checkAll()
	if r.c.st.Registries["ghcr.io"] != nil || r.c.st.Refs["ghcr.io/x/a:1"].Fails != 0 {
		t.Fatal("success must reset")
	}
	// A failure after a success keeps the digest and mode checked.
	r.f.distErr["ghcr.io/x/a:1"] = errBoom
	r.clk.add(2 * time.Hour)
	r.checkAll()
	r.reconcile()
	r.has(`hatchlens_container_image_info{container="a",mode="checked",reference="ghcr.io/x/a:1",state="running"} 1`,
		`hatchlens_container_image_update_available{container="a",image="ghcr.io/x/a:1"} 0`)
	// 200 without a digest.
	r.f.dist["ghcr.io/x/b:1"] = ""
	r.clk.add(2 * time.Hour)
	r.checkAll()
	if r.c.st.Refs["ghcr.io/x/b:1"].Reason != "unreachable" {
		t.Fatal("empty digest")
	}
}

func TestBackoffLimit(t *testing.T) {
	if backoff(0, time.Hour) != 5*time.Minute || backoff(2, time.Hour) != 20*time.Minute ||
		backoff(10, time.Hour) != time.Hour || backoff(100, time.Hour) != time.Hour || backoff(0, time.Minute) != time.Minute {
		t.Fatal("backoff")
	}
	if reason(&docker.APIError{Status: 404}) != "not_found" || reason(&docker.APIError{Status: 401}) != "denied" ||
		reason(&docker.APIError{Status: 500}) != "unreachable" {
		t.Fatal("reason")
	}
}

func TestForget(t *testing.T) {
	r := newRig(t, "images: { forget_after: 1h }")
	r.f.add("web", "nginx:1", "sha256:old", "running", nil)
	r.f.dist["docker.io/library/nginx:1"] = "sha256:new"
	r.reconcile()
	r.checkAll()
	r.reconcile()
	if len(r.c.st.Since) != 1 {
		t.Fatal("since")
	}
	// compose down
	r.f.containers = nil
	r.reconcile()
	r.clk.add(30 * time.Minute)
	r.reconcile()
	r.c.CheckDue(context.Background())
	if len(r.c.st.Refs) != 1 || len(r.c.st.Since) != 1 {
		t.Fatal("forgotten too early")
	}
	// up again within forget_after: since continues
	r.f.add("web", "nginx:1", "sha256:old", "running", nil)
	r.reconcile()
	if r.c.st.Since[state.SinceKey("web", "docker.io/library/nginx:1")].Unused != 0 {
		t.Fatal("unused not cleared")
	}
	r.f.containers = nil
	r.reconcile()
	r.clk.add(2 * time.Hour)
	r.reconcile()
	if len(r.c.st.Refs) != 0 || len(r.c.st.Since) != 0 {
		t.Fatalf("not forgotten: %v %v", r.c.st.Refs, r.c.st.Since)
	}
}

func TestReconcileErrors(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, "")
	r.f.add("web", "nginx:1", "sha256:a", "running", nil)
	for name, set := range map[string]func(){
		"unreachable": func() { r.f.verErr, r.f.reached = errBoom, false },
		"list":        func() { r.f.listErr = errBoom },
		"inspect":     func() { r.f.inspectErr = errBoom },
		"image":       func() { r.f.imageErr["sha256:a"] = errBoom },
		"ref image":   func() { r.f.imageErr["docker.io/library/nginx:1"] = errBoom },
	} {
		r.f = newFake()
		r.f.add("web", "nginx:1", "sha256:a", "running", nil)
		set()
		r.start()
		before := r.c.recErrors
		if err := r.c.Reconcile(ctx); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.c.recErrors != before+1 || r.c.Ready() {
			t.Errorf("%s: not counted", name)
		}
	}
	// Cut off by the shutdown: not a failure.
	r.f = newFake()
	r.f.listErr = context.Canceled
	r.start()
	r.log.Reset()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.c.Reconcile(canceled); err != nil || r.c.recErrors != 0 || strings.Contains(r.log.String(), "reconcile failed") {
		t.Fatalf("shutdown counted: %v %v", err, r.c.recErrors)
	}
	r.f = newFake()
	r.f.verErr = errBoom
	r.start()
	if err := r.c.Reconcile(ctx); err == nil {
		t.Fatal("too old must stop")
	}
	n := New(config.Default(), Options{StorageDir: t.TempDir(), Log: quiet()})
	if err := n.Reconcile(ctx); err != nil || n.recErrors != 1 {
		t.Fatal("no Docker")
	}
	if n.CheckDue(ctx) != idleWait {
		t.Fatal("no Docker: checks idle")
	}
	if _, errs := r.c.Stats(); errs != 0 {
		t.Fatal("reconcile errors")
	}
}

func TestCheckDueEdges(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, "")
	if r.c.CheckDue(ctx) != idleWait {
		t.Fatal("not ready")
	}
	r.f.add("web", "nginx:1", "sha256:a", "running", nil)
	r.f.add("app", "ghcr.io/x/app:1", "sha256:b", "running", nil)
	r.f.dist["docker.io/library/nginx:1"] = "sha256:a"
	r.f.dist["ghcr.io/x/app:1"] = "sha256:b"
	r.reconcile()
	// Two references of different registries go one after the other; the same registry waits 10s.
	r.c.CheckDue(ctx)
	r.c.CheckDue(ctx)
	if len(r.f.distRefs) != 2 {
		t.Fatal(r.f.distRefs)
	}
	if w := r.c.CheckDue(ctx); w <= 0 || w > idleWait {
		t.Fatalf("wait %v", w)
	}
	// A reference forgotten while its check runs.
	r.f.add("x", "ghcr.io/x/x:1", "sha256:x", "running", nil)
	r.reconcile()
	r.clk.add(time.Minute)
	r.f.onDist = func() {
		r.c.mu.Lock()
		delete(r.c.st.Refs, "ghcr.io/x/x:1")
		r.c.mu.Unlock()
	}
	if r.c.CheckDue(ctx) != 0 {
		t.Fatal("forgotten ref")
	}
	r.f.onDist = nil
	// Registry backoff in the future delays the pick.
	r.c.st.Registries["ghcr.io"] = &state.Registry{Until: r.clk.now().Unix() + 30}
	r.c.st.Refs["ghcr.io/x/app:1"].Due = 0
	if w := r.c.CheckDue(ctx); w != 30*time.Second {
		t.Fatalf("registry wait %v", w)
	}
}

func TestRunChecks(t *testing.T) {
	r := newRig(t, "")
	r.f.add("web", "nginx:1", "sha256:a", "running", nil)
	r.f.dist["docker.io/library/nginx:1"] = "sha256:a"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.c.RunChecks(ctx); close(done) }()
	r.reconcile()
	for i := 0; i < 200; i++ {
		r.c.mu.Lock()
		rs := r.c.st.Refs["docker.io/library/nginx:1"]
		ok := rs != nil && rs.Digest != ""
		r.c.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-r.c.ReconcileRequests():
	case <-time.After(2 * time.Second):
		t.Fatal("no reconcile request")
	}
	cancel()
	<-done
}

func TestStateErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, state.FileName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, "")
	r.dir = dir
	r.start()
	if !strings.Contains(r.log.String(), "state reset") {
		t.Fatal("corrupt state not logged")
	}
	// Not writable: the parent of the storage is a file; logged once per hour.
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0o644)
	r.dir = filepath.Join(file, "sub")
	r.start()
	r.f.add("web", "nginx:1", "sha256:a", "running", nil)
	r.reconcile()
	r.f.add("web2", "nginx:2", "sha256:b", "running", nil)
	r.reconcile()
	if n := strings.Count(r.log.String(), "writing state failed"); n != 1 {
		t.Fatalf("logged %d times", n)
	}
	r.clk.add(time.Hour)
	r.f.add("web3", "nginx:3", "sha256:c", "running", nil)
	r.reconcile()
	if n := strings.Count(r.log.String(), "writing state failed"); n != 2 {
		t.Fatalf("logged %d times", n)
	}
}

func TestConfigAndDefaults(t *testing.T) {
	c := New(config.Default(), Options{StorageDir: t.TempDir()})
	if c.Config() == nil {
		t.Fatal("config not set")
	}
	if stateLabel("dead") != "stopped" || stateLabel("restarting") != "running" {
		t.Fatal("stateLabel")
	}
	if k := sinceKey(docker.Container{Names: []string{"/x"}, Labels: map[string]string{"com.docker.swarm.service.name": "s"}}); k != "x" {
		t.Fatal(k)
	}
	if c.rand(10) < 0 || c.rand(10) >= 10 {
		t.Fatal("rand")
	}
}

// Containers removed between the list and the inspect are skipped, not a failed
// reconcile; a container whose image was removed (rmi -f) is reported by its image ID.
func TestVanishedContainers(t *testing.T) {
	r := newRig(t, "")
	r.f.add("web", "nginx:1", "sha256:a", "running", nil)
	r.f.add("gone", "nginx:1", "sha256:a", "running", nil)
	r.f.add("noimage", "redis:7", "sha256:b", "exited", nil)
	r.f.gone["c-gone"] = true
	delete(r.f.images, "sha256:b")
	r.f.dist["docker.io/library/redis:7"] = "sha256:r"
	r.reconcile()
	if r.c.recErrors != 0 || !r.c.Ready() {
		t.Fatal("vanished containers failed the reconcile")
	}
	r.has(`hatchlens_container_image_info{container="web",mode="pending",reference="docker.io/library/nginx:1",state="running"} 1`,
		`hatchlens_container_image_info{container="noimage",mode="pending",reference="docker.io/library/redis:7",state="stopped"} 1`,
		`hatchlens_container_image_update_pulled{container="noimage",image="redis:7"} 0`)
	r.hasNot(`container="gone"`)
	r.checkAll()
	r.clk.add(10 * time.Second)
	r.checkAll()
	r.reconcile()
	r.has(`hatchlens_container_image_update_available{container="noimage",image="redis:7"} 1`)
}

// Removed containers leave nothing behind in the change log and warning memory.
func TestForgetRemovedContainers(t *testing.T) {
	r := newRig(t, "")
	r.f.add("web", "nginx:1", "sha256:a", "running", map[string]string{labelSelf + "bad-name": "x"})
	r.f.add("db", "redis:7", "sha256:b", "running", nil)
	r.reconcile()
	if len(r.c.prev) != 2 || len(r.c.warned) != 1 {
		t.Fatalf("prev %d warned %d", len(r.c.prev), len(r.c.warned))
	}
	r.f.containers = r.f.containers[1:]
	r.reconcile()
	if _, ok := r.c.prev["web"]; ok || len(r.c.prev) != 1 || len(r.c.warned) != 0 {
		t.Fatalf("prev %v warned %v", r.c.prev, r.c.warned)
	}
}

// A check cut off by the shutdown is not recorded as a failure.
func TestShutdownDuringCheck(t *testing.T) {
	r := newRig(t, "")
	r.f.add("web", "nginx:1", "sha256:a", "running", nil, "docker.io/library/nginx@sha256:a")
	r.f.dist["docker.io/library/nginx:1"] = "sha256:a"
	r.reconcile()
	r.checkAll()
	ctx, cancel := context.WithCancel(context.Background())
	r.f.onDist = cancel
	r.f.distErr["docker.io/library/nginx:1"] = context.Canceled
	r.clk.add(48 * time.Hour)
	r.c.CheckDue(ctx)
	if rs := r.c.st.Refs["docker.io/library/nginx:1"]; rs.Reason != "" || rs.Fails != 0 || rs.Digest != "sha256:a" {
		t.Fatalf("recorded as failure: %+v", rs)
	}
	r.hasNot(`image_check_error`)
}

func TestRecheckAfterImageSwap(t *testing.T) {
	r := newRig(t, "images: { check_interval: 24h }")
	r.f.add("web", "nginx:1.29", "sha256:old", "running", nil, "nginx@sha256:idx0")
	r.f.dist[nginx] = "sha256:idx1"
	r.reconcile()
	r.checkAll()
	r.reconcile()
	r.has(`hatchlens_container_image_update_available{container="web",image="nginx:1.29"} 1`)

	// The registry moved on to idx2 unseen; the container is recreated on it.
	r.f.dist[nginx] = "sha256:idx2"
	r.clk.add(time.Minute)
	r.f.containers[0].ImageID = "sha256:new"
	r.f.images["sha256:new"] = docker.Image{ID: "sha256:new", RepoDigests: []string{"nginx@sha256:idx2"}}
	r.reconcile()
	if rs := r.c.st.Refs[nginx]; rs.Due != r.clk.now().Unix() {
		t.Fatalf("not due after the swap: %d", rs.Due)
	}
	calls := len(r.f.distRefs)
	r.checkAll()
	if len(r.f.distRefs) != calls+1 {
		t.Fatal("no recheck")
	}
	r.reconcile()
	r.has(`hatchlens_container_image_update_available{container="web",image="nginx:1.29"} 0`)
	r.hasNot("since_timestamp_seconds{")

	// Unchanged running digests and a failing reference keep their due time.
	r.f.dist[nginx] = "sha256:idx3"
	r.clk.add(25 * time.Hour)
	r.checkAll()
	r.reconcile()
	due := r.c.st.Refs[nginx].Due
	r.reconcile()
	if r.c.st.Refs[nginx].Due != due {
		t.Fatal("due changed without a swap")
	}
	r.f.distErr[nginx] = errBoom
	r.clk.add(25 * time.Hour)
	r.checkAll()
	due = r.c.st.Refs[nginx].Due
	r.f.containers[0].ImageID = "sha256:other"
	r.f.images["sha256:other"] = docker.Image{ID: "sha256:other"}
	r.reconcile()
	if r.c.st.Refs[nginx].Due != due {
		t.Fatal("backoff skipped")
	}
}
