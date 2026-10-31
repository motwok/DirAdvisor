// SPDX-License-Identifier: AGPL-3.0-or-later

package docker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type rt struct{ h http.Handler }

func (r rt) RoundTrip(req *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w.Result(), nil
}

func fake(api string) *Client {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ApiVersion":"`+api+`"}`)
	})
	mux.HandleFunc("/v1.41/info", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"DockerRootDir":"/var/lib/docker"}`)
	})
	mux.HandleFunc("/v1.41/volumes", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Volumes":[{"Name":"v","Driver":"local","Mountpoint":"/var/lib/docker/volumes/v/_data","Labels":{"a":"b"}}]}`)
	})
	mux.HandleFunc("/v1.41/containers/json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("all") != "1" {
			http.Error(w, "all missing", 400)
			return
		}
		io.WriteString(w, `[{"Id":"abc","Names":["/web-1"],"State":"running","Created":5,"Mounts":[{"Type":"volume","Name":"v","RW":true}]}]`)
	})
	mux.HandleFunc("/v1.41/containers/abc/json", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Id":"abc","Image":"sha256:1","Config":{"Image":"ghcr.io/motwok/diradvisor:latest"}}`)
	})
	mux.HandleFunc("/v1.41/images/sha256:1/json", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"RepoDigests":["ghcr.io/motwok/diradvisor@sha256:aa"]}`)
	})
	mux.HandleFunc("/v1.41/distribution/", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Descriptor":{"digest":"sha256:bb"}}`)
	})
	mux.HandleFunc("/v1.41/events", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Type":"volume","Action":"create"}`+"\n"+`{"Type":"container","Action":"exec_start"}`+"\n")
	})
	mux.HandleFunc("/v1.41/containers/missing/json", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		io.WriteString(w, `{"message":"No such container"}`)
	})
	return NewWithHTTP(rt{mux})
}

func TestClient(t *testing.T) {
	ctx := context.Background()
	c := fake("1.47")
	if v, reached, err := c.CheckVersion(ctx); err != nil || !reached || v != "1.47" {
		t.Fatalf("version: %v %v %v", v, reached, err)
	}
	if i, err := c.Info(ctx); err != nil || i.DockerRootDir != "/var/lib/docker" {
		t.Fatalf("info: %v %v", i, err)
	}
	if v, err := c.Volumes(ctx); err != nil || len(v) != 1 || v[0].Labels["a"] != "b" {
		t.Fatalf("volumes: %v %v", v, err)
	}
	cs, err := c.Containers(ctx)
	if err != nil || len(cs) != 1 || cs[0].Name() != "web-1" || !cs[0].Mounts[0].RW {
		t.Fatalf("containers: %v %v", cs, err)
	}
	if j, err := c.Container(ctx, "abc"); err != nil || j.Config.Image != "ghcr.io/motwok/diradvisor:latest" {
		t.Fatalf("container: %v %v", j, err)
	}
	if d, err := c.ImageRepoDigests(ctx, "sha256:1"); err != nil || d[0] != "ghcr.io/motwok/diradvisor@sha256:aa" {
		t.Fatalf("digests: %v %v", d, err)
	}
	if d, err := c.DistributionDigest(ctx, "ghcr.io/motwok/diradvisor:latest"); err != nil || d != "sha256:bb" {
		t.Fatalf("distribution: %v %v", d, err)
	}
	_, err = c.Container(ctx, "missing")
	if e, ok := err.(*APIError); !ok || e.Status != 404 || !strings.Contains(e.Error(), "No such container") {
		t.Fatalf("api error: %v", err)
	}
	var got []Event
	err = c.Events(ctx, func(e Event) { got = append(got, e) })
	if err != io.EOF || len(got) != 2 || !got[0].Relevant() || got[1].Relevant() {
		t.Fatalf("events: %v %v", got, err)
	}
}

func TestOldDaemon(t *testing.T) {
	if _, reached, err := fake("1.40").CheckVersion(context.Background()); err == nil || !reached {
		t.Fatalf("expected version error, got %v", err)
	}
}

func TestCompareVersion(t *testing.T) {
	if CompareVersion("1.41", "1.41") != 0 || CompareVersion("1.9", "1.41") >= 0 || CompareVersion("1.47", "1.41") <= 0 ||
		CompareVersion("2", "1.99") <= 0 {
		t.Fatal("CompareVersion")
	}
}

func TestRelevant(t *testing.T) {
	for _, e := range []Event{{"volume", "destroy"}, {"container", "die"}, {"container", "start"}} {
		if !e.Relevant() {
			t.Errorf("%v should be relevant", e)
		}
	}
	for _, e := range []Event{{"volume", "mount"}, {"network", "create"}} {
		if e.Relevant() {
			t.Errorf("%v should not be relevant", e)
		}
	}
	if (Container{ID: "x"}).Name() != "x" {
		t.Fatal("name fallback")
	}
}

type errRT struct{}

func (errRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, errors.New("dial failed") }

func TestErrors(t *testing.T) {
	var nilCtx context.Context
	c := fake("1.47")
	if _, err := c.Info(nilCtx); err == nil {
		t.Error("get: bad request")
	}
	if _, _, err := c.CheckVersion(nilCtx); err == nil {
		t.Error("version: bad request")
	}
	if err := c.Events(nilCtx, nil); err == nil {
		t.Error("events: bad request")
	}

	ctx := context.Background()
	d := NewWithHTTP(errRT{})
	if _, err := d.Info(ctx); err == nil {
		t.Error("get: dial")
	}
	if _, reached, err := d.CheckVersion(ctx); err == nil || reached {
		t.Error("version: dial")
	}
	if err := d.Events(ctx, nil); err == nil {
		t.Error("events: dial")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "plain failure\n")
	})
	e := NewWithHTTP(rt{mux})
	if _, err := e.Info(ctx); err == nil || err.(*APIError).Message != "plain failure" {
		t.Errorf("plain error: %v", err)
	}
	if _, reached, err := e.CheckVersion(ctx); err == nil || !reached {
		t.Errorf("version: bad body: %v", err)
	}
	if err := e.Events(ctx, nil); err == nil || err.(*APIError).Status != 500 {
		t.Errorf("events: status: %v", err)
	}
}

func TestUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ApiVersion":"1.47"}`)
	}))
	srv.Listener = l
	srv.Start()
	defer srv.Close()
	if v, _, err := New(sock).CheckVersion(context.Background()); err != nil || v != "1.47" {
		t.Fatalf("%q %v", v, err)
	}
}
