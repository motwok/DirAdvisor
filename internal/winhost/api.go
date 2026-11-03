// SPDX-License-Identifier: AGPL-3.0-or-later

// Package winhost is the HatchLens Host service on Windows: it answers the
// container over a named pipe with what only the host can see, the HNS
// endpoints and the sizes of directory trees.
package winhost

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
)

// Pipe is the named pipe the service listens on.
const Pipe = `\\.\pipe\hatchlens-host`

// Endpoint is an HNS endpoint: the key of the Docker stats network counters
// and the HNS network it belongs to.
type Endpoint struct {
	ID      string `json:"id"`
	Network string `json:"network"`
}

// Mount is the volume of a path. Status is "" when it can be measured, else
// the reason (filesystem_not_supported, enable_usn_journal).
type Mount struct {
	Point  string `json:"point"`
	FSType string `json:"fstype"`
	Status string `json:"status"`
}

type Usage struct {
	Bytes  uint64 `json:"bytes"`
	Inodes uint64 `json:"inodes"`
}

type Stat struct {
	Exists bool `json:"exists"`
	Dir    bool `json:"dir"`
}

// Backend does the work; Handler only translates HTTP.
type Backend interface {
	Endpoints() ([]Endpoint, error)
	Stat(path string) Stat
	Dirs(path string) ([]string, error)
	Mount(path string) (Mount, error)
	ID(path string) (uint32, error)
	// Assign counts the tree below path as id (0: not at all) and returns once
	// it has been walked.
	Assign(ctx context.Context, path string, id uint32) error
	Usage(volume string, min uint32) (map[uint32]Usage, error)
}

type assignRequest struct {
	Path string `json:"path"`
	ID   uint32 `json:"id"`
}

// Handler serves the service API.
func Handler(b Backend, version string) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any, err error) {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			http.Error(w, err.Error(), http.StatusNotFound)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(v)
		}
	}
	path := func(r *http.Request) string { return r.URL.Query().Get("path") }
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, map[string]string{"version": version}, nil)
	})
	mux.HandleFunc("GET /v1/endpoints", func(w http.ResponseWriter, _ *http.Request) {
		eps, err := b.Endpoints()
		reply(w, eps, err)
	})
	mux.HandleFunc("GET /v1/stat", func(w http.ResponseWriter, r *http.Request) {
		reply(w, b.Stat(path(r)), nil)
	})
	mux.HandleFunc("GET /v1/dirs", func(w http.ResponseWriter, r *http.Request) {
		d, err := b.Dirs(path(r))
		reply(w, d, err)
	})
	mux.HandleFunc("GET /v1/mount", func(w http.ResponseWriter, r *http.Request) {
		m, err := b.Mount(path(r))
		reply(w, m, err)
	})
	mux.HandleFunc("GET /v1/id", func(w http.ResponseWriter, r *http.Request) {
		id, err := b.ID(path(r))
		reply(w, map[string]uint32{"id": id}, err)
	})
	mux.HandleFunc("POST /v1/assign", func(w http.ResponseWriter, r *http.Request) {
		var req assignRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply(w, map[string]bool{"ok": true}, b.Assign(r.Context(), req.Path, req.ID))
	})
	mux.HandleFunc("GET /v1/usage", func(w http.ResponseWriter, r *http.Request) {
		min, _ := strconv.ParseUint(r.URL.Query().Get("min"), 10, 32)
		u, err := b.Usage(r.URL.Query().Get("volume"), uint32(min))
		out := map[string]Usage{}
		for id, x := range u {
			out[strconv.FormatUint(uint64(id), 10)] = x
		}
		reply(w, out, err)
	})
	return mux
}
