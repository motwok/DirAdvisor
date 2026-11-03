// SPDX-License-Identifier: AGPL-3.0-or-later

package docker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
		io.WriteString(w, `{"ApiVersion":"`+api+`","MinAPIVersion":"1.12"}`)
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
		io.WriteString(w, `[{"Id":"abc","Names":["/web-1"],"ImageID":"sha256:1","State":"running","Labels":{"a":"b"},"Created":5,"Mounts":[{"Type":"volume","Name":"v","RW":true}],`+
			`"HostConfig":{"NetworkMode":"app_default"},"NetworkSettings":{"Networks":{"app_default":`+
			`{"EndpointID":"e1","IPAddress":"172.18.0.2","IPPrefixLen":16,"GlobalIPv6Address":"fd00::2","GlobalIPv6PrefixLen":64}}}}]`)
	})
	mux.HandleFunc("/v1.41/containers/abc/json", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Id":"abc","Image":"sha256:1","Config":{"Image":"nginx:1.29"},"State":{"Pid":42},`+
			`"HostConfig":{"NanoCpus":500000000,"Memory":1048576}}`)
	})
	mux.HandleFunc("/v1.41/containers/abc/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") != "false" || r.URL.Query().Get("one-shot") != "true" {
			http.Error(w, "query", 400)
			return
		}
		io.WriteString(w, `{"cpu_stats":{"cpu_usage":{"total_usage":30000000}},`+
			`"memory_stats":{"commitbytes":2048,"privateworkingset":1024},`+
			`"storage_stats":{"read_size_bytes":1,"write_size_bytes":2,"read_count_normalized":3,"write_count_normalized":4},`+
			`"networks":{"e1":{"rx_bytes":5,"rx_packets":6,"rx_errors":7,"rx_dropped":8,"tx_bytes":9,"tx_packets":10,"tx_errors":11,"tx_dropped":12}}}`)
	})
	mux.HandleFunc("/v1.41/images/sha256:1/json", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Id":"sha256:1","RepoDigests":["nginx@sha256:aa"]}`)
	})
	mux.HandleFunc("/v1.41/images/docker.io/library/nginx:1.29/json", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Id":"sha256:2"}`)
	})
	mux.HandleFunc("/v1.41/distribution/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.41/distribution/docker.io/library/nginx:1.29/json" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"Descriptor":{"digest":"sha256:bb"},"auth":"`+r.Header.Get("X-Registry-Auth")+`"}`)
	})
	mux.HandleFunc("/v1.41/events", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("filters"), `"image"`) || !strings.Contains(r.URL.Query().Get("filters"), `"network"`) {
			http.Error(w, "filters", 400)
			return
		}
		io.WriteString(w, `{"Type":"image","Action":"pull"}`+"\n"+`{"Type":"volume","Action":"create"}`+"\n"+
			`{"Type":"container","Action":"exec_start"}`+"\n")
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
	if err != nil || len(cs) != 1 || cs[0].Name() != "web-1" || cs[0].ImageID != "sha256:1" || cs[0].Labels["a"] != "b" ||
		!cs[0].Mounts[0].RW || cs[0].Created != 5 || cs[0].HostConfig.NetworkMode != "app_default" ||
		cs[0].NetworkSettings.Networks["app_default"] != (Endpoint{"e1", "172.18.0.2", 16, "fd00::2", 64}) {
		t.Fatalf("containers: %v %v", cs, err)
	}
	if j, err := c.Container(ctx, "abc"); err != nil || j.Config.Image != "nginx:1.29" || j.Image != "sha256:1" ||
		j.State.Pid != 42 || j.HostConfig.NanoCpus != 5e8 || j.HostConfig.Memory != 1<<20 {
		t.Fatalf("container: %v %v", j, err)
	}
	st, err := c.Stats(ctx, "abc")
	if err != nil || st.CPUStats.CPUUsage.TotalUsage != 3e7 || st.MemoryStats.Commit != 2048 ||
		st.MemoryStats.PrivateWorkingSet != 1024 || st.StorageStats.Writes != 4 ||
		st.Networks["e1"] != (NetStats{5, 6, 7, 8, 9, 10, 11, 12}) {
		t.Fatalf("stats: %+v %v", st, err)
	}
	if im, err := c.Image(ctx, "sha256:1"); err != nil || im.ID != "sha256:1" || im.RepoDigests[0] != "nginx@sha256:aa" {
		t.Fatalf("image: %v %v", im, err)
	}
	if im, err := c.Image(ctx, "docker.io/library/nginx:1.29"); err != nil || im.ID != "sha256:2" {
		t.Fatalf("image by ref: %v %v", im, err)
	}
	if d, err := c.Distribution(ctx, "docker.io/library/nginx:1.29", ""); err != nil || d != "sha256:bb" {
		t.Fatalf("distribution: %v %v", d, err)
	}
	if d, err := c.Distribution(ctx, "docker.io/library/nginx:1.29", "tok"); err != nil || d != "sha256:bb" {
		t.Fatalf("distribution with auth: %v %v", d, err)
	}
	_, err = c.Distribution(ctx, "docker.io/library/x:1", "")
	if e, ok := err.(*APIError); !ok || e.Status != 404 {
		t.Fatalf("distribution 404: %v", err)
	}
	_, err = c.Container(ctx, "missing")
	if e, ok := err.(*APIError); !ok || e.Status != 404 || !strings.Contains(e.Error(), "No such container") {
		t.Fatalf("api error: %v", err)
	}
	var got []Event
	err = c.Events(ctx, func(e Event) { got = append(got, e) })
	if err != io.EOF || len(got) != 3 || !got[0].Relevant() || !got[1].Relevant() || got[2].Relevant() {
		t.Fatalf("events: %v %v", got, err)
	}
}

func TestAuthHeader(t *testing.T) {
	var seen string
	c := NewWithHTTP(rt{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Registry-Auth")
		io.WriteString(w, `{"Descriptor":{"digest":"d"}}`)
	})})
	c.Distribution(context.Background(), "r", "tok")
	if seen != "tok" {
		t.Fatalf("header %q", seen)
	}
	c.Distribution(context.Background(), "r", "")
	if seen != "" {
		t.Fatalf("header without auth %q", seen)
	}
}

func TestOldDaemon(t *testing.T) {
	if _, reached, err := fake("1.40").CheckVersion(context.Background()); err == nil || !reached {
		t.Fatalf("expected version error, got %v", err)
	}
}

// A daemon whose minimum API version is above 1.41 gets its minimum in the path.
func TestMinAPIVersion(t *testing.T) {
	ctx := context.Background()
	var paths []string
	c := NewWithHTTP(rt{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/version" {
			io.WriteString(w, `{"ApiVersion":"1.52","MinAPIVersion":"1.44"}`)
			return
		}
		io.WriteString(w, `[]`)
	})})
	c.Containers(ctx)
	if v, reached, err := c.CheckVersion(ctx); err != nil || !reached || v != "1.52" {
		t.Fatalf("version: %v %v %v", v, reached, err)
	}
	c.Containers(ctx)
	c.Events(ctx, func(Event) {})
	want := "/v1.41/containers/json /version /v1.44/containers/json /v1.44/events"
	if got := strings.Join(paths, " "); got != want {
		t.Fatalf("paths %q", got)
	}
	// A minimum at or below 1.41 keeps 1.41.
	old := fake("1.47")
	old.CheckVersion(ctx)
	if old.apiVersion() != MinAPIVersion {
		t.Fatalf("version %s", old.apiVersion())
	}
}

func TestCompareVersion(t *testing.T) {
	if CompareVersion("1.41", "1.41") != 0 || CompareVersion("1.9", "1.41") >= 0 || CompareVersion("1.47", "1.41") <= 0 ||
		CompareVersion("2", "1.99") <= 0 {
		t.Fatal("CompareVersion")
	}
}

func TestRelevant(t *testing.T) {
	for _, e := range []Event{{"container", "create"}, {"container", "start"}, {"container", "stop"},
		{"container", "die"}, {"container", "destroy"}, {"image", "pull"}, {"image", "tag"},
		{"image", "untag"}, {"image", "delete"}, {"volume", "create"}, {"volume", "destroy"},
		{"network", "connect"}, {"network", "disconnect"}} {
		if !e.Relevant() {
			t.Errorf("%v should be relevant", e)
		}
	}
	for _, e := range []Event{{"container", "exec_start"}, {"image", "push"}, {"network", "create"}, {"volume", "mount"}} {
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
	if _, err := c.Containers(nilCtx); err == nil {
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
	if _, err := d.Containers(ctx); err == nil {
		t.Error("get: dial")
	}
	if _, reached, err := d.CheckVersion(ctx); err == nil || reached {
		t.Error("version: dial")
	}
	if err := d.Events(ctx, nil); err == nil {
		t.Error("events: dial")
	}

	e := NewWithHTTP(rt{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "plain failure\n")
	})})
	if _, err := e.Containers(ctx); err == nil || err.(*APIError).Message != "plain failure" {
		t.Errorf("plain error: %v", err)
	}
	if _, reached, err := e.CheckVersion(ctx); err == nil || reached || err.(*APIError).Status != 500 {
		t.Errorf("version: status: %v", err)
	}
	g := NewWithHTTP(rt{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "not json")
	})})
	if _, reached, err := g.CheckVersion(ctx); err == nil || reached {
		t.Errorf("version: bad body: %v", err)
	}
	if err := e.Events(ctx, nil); err == nil || err.(*APIError).Status != 500 {
		t.Errorf("events: status: %v", err)
	}
}

func TestNameSkipsLinkAliases(t *testing.T) {
	for names, want := range map[string]string{
		"/app/web,/web": "web",
		"/web":          "web",
		"/app/web":      "app/web",
		"":              "id",
	} {
		c := Container{ID: "id"}
		if names != "" {
			c.Names = strings.Split(names, ",")
		}
		if got := c.Name(); got != want {
			t.Errorf("%q: got %q, want %q", names, got, want)
		}
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(&APIError{Status: 404}) || IsNotFound(&APIError{Status: 500}) || IsNotFound(errors.New("x")) {
		t.Fatal("IsNotFound")
	}
}
