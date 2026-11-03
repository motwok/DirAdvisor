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
	"github.com/motwok/HatchLens/internal/usage"
)

// statsEngine answers the container part for one running container "web".
func statsEngine(api string) *docker.Client {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ApiVersion":"`+api+`"}`)
	})
	mux.HandleFunc("/v1.41/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `[{"Id":"c1","Names":["/web"],"ImageID":"sha256:1","State":"running"}]`)
	})
	mux.HandleFunc("/v1.41/containers/c1/json", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"Id":"c1","Config":{"Image":"nginx:1"}}`)
	})
	mux.HandleFunc("/v1.41/containers/c1/stats", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"cpu_stats":{"cpu_usage":{"total_usage":10000000}}}`)
	})
	return docker.NewWithHTTP(handlerRT{mux})
}

func usageServer(t *testing.T, api, cfgText string) *Server {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgText), os.LookupEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.Usage = usage.New(cfg, usage.Options{Docker: statsEngine(api), Windows: true, Log: s.Log})
	return s
}

func TestUsagePart(t *testing.T) {
	s := usageServer(t, "1.47", "reconcile_interval: 5ms")
	h := s.Handler()
	if code, _ := get(t, h, "GET", "/-/ready"); code != 503 {
		t.Fatal("ready before reconcile")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- s.run(ctx, cancel) }()
	seen := func() bool {
		return strings.Contains(metricsOf(t, s), `hatchlens_container_cpu_usage_seconds_total{container="web"} 1`)
	}
	for i := 0; i < 200 && !seen(); i++ {
		s.poke()
		time.Sleep(5 * time.Millisecond)
	}
	if !seen() {
		t.Fatalf("no CPU series:\n%s", metricsOf(t, s))
	}
	if code, _ := get(t, h, "GET", "/-/ready"); code != 200 {
		t.Fatal("ready after reconcile")
	}
	if body := metricsOf(t, s); !strings.Contains(body, "hatchlens_reconcile_errors_total 0") {
		t.Fatalf("shared metrics:\n%s", body)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestUsageLoop(t *testing.T) {
	s := usageServer(t, "1.40", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if code := s.run(ctx, cancel); code != 1 {
		t.Fatalf("too old: exit %d", code)
	}
	s = usageServer(t, "1.47", "")
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if code := s.usageLoop(ctx); code != 0 {
		t.Fatalf("canceled: exit %d", code)
	}
	go func() { time.Sleep(10 * time.Millisecond); s.poke() }()
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if code := s.usageLoop(ctx); code != 0 {
		t.Fatalf("exit %d", code)
	}
}
