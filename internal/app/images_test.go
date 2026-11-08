// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/images"
)

// fakeEngine answers the requests of the image part for one container "web" from nginx:1.
func fakeEngine(api string) *docker.Client {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ApiVersion":"`+api+`"}`)
	})
	mux.HandleFunc("/v1.41/info", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/v1.41/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `[{"Id":"c1","Names":["/web"],"ImageID":"sha256:1","State":"running"}]`)
	})
	mux.HandleFunc("/v1.41/containers/c1/json", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Id":"c1","Image":"sha256:1","Config":{"Image":"nginx:1"}}`)
	})
	mux.HandleFunc("/v1.41/images/", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Id":"sha256:1","RepoDigests":["nginx@sha256:aa"]}`)
	})
	mux.HandleFunc("/v1.41/distribution/", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Descriptor":{"digest":"sha256:aa"}}`)
	})
	return docker.NewWithHTTP(handlerRT{mux})
}

func imageServer(t *testing.T, api, cfgText string) *Server {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgText), os.LookupEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.Images = images.New(cfg, images.Options{Docker: fakeEngine(api), StorageDir: t.TempDir(), Log: s.Log})
	return s
}

func TestImagePart(t *testing.T) {
	s := imageServer(t, "1.47", "reconcile_interval: 5ms")
	h := s.Handler()
	if code, _ := get(t, h, "GET", "/-/ready"); code != 503 {
		t.Fatal("ready before reconcile")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- s.run(ctx, cancel) }()
	checked := func() bool {
		return strings.Contains(metricsOf(t, s), `hatchlens_container_image_update_available{container="web",image="nginx:1"} 0`)
	}
	for i := 0; i < 200 && !checked(); i++ {
		s.poke()
		time.Sleep(5 * time.Millisecond)
	}
	if !checked() {
		t.Fatalf("not checked:\n%s", metricsOf(t, s))
	}
	if code, _ := get(t, h, "GET", "/-/ready"); code != 200 {
		t.Fatal("ready after reconcile")
	}
	body := metricsOf(t, s)
	for _, want := range []string{"hatchlens_reconcile_errors_total 0", "hatchlens_build_info{", `hatchlens_image_check_success{reference="docker.io/library/nginx:1"} 1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s:\n%s", want, body)
		}
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestImageLoopDockerTooOld(t *testing.T) {
	s := imageServer(t, "1.40", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if code := s.run(ctx, cancel); code != 1 {
		t.Fatalf("exit %d", code)
	}
}

func TestImageLoopStopsOnCancelledContext(t *testing.T) {
	s := imageServer(t, "1.47", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := s.imageLoop(ctx); code != 0 {
		t.Fatalf("exit %d", code)
	}
	go func() { time.Sleep(10 * time.Millisecond); s.poke() }()
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if code := s.imageLoop(ctx); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// Without any part the server just waits for the end.
func TestRunWithoutParts(t *testing.T) {
	s := newServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := s.run(ctx, cancel); code != 0 || !s.ready() {
		t.Fatalf("exit %d", code)
	}
}
