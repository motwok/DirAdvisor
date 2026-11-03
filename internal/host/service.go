// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/motwok/HatchLens/internal/config"
)

// Service is the FS on Windows: the HatchLens Host service on the host answers
// over a named pipe. A file counts for the innermost assigned directory there,
// as a file carries one project ID on Linux. Paths go out in Windows form.
type Service struct {
	hc, walk *http.Client
}

func NewService(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *Service {
	t := &http.Transport{DialContext: dial}
	return &Service{hc: &http.Client{Timeout: 30 * time.Second, Transport: t}, walk: &http.Client{Transport: t}}
}

func (s *Service) call(ctx context.Context, c *http.Client, method, p string, q url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://hatchlens-host"+p+"?"+q.Encode(), rd)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fs.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("host service %s: %d %s", p, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *Service) get(p, path string, out any) error {
	return s.call(context.Background(), s.hc, http.MethodGet, p, url.Values{"path": {config.NativePath(path)}}, nil, out)
}

// Stat reports an unreachable service as an existing directory, so the entry
// goes on to MountOf and shows install_host_service instead of path_missing.
func (s *Service) Stat(p string) (bool, bool) {
	var st struct{ Exists, Dir bool }
	if err := s.get("/v1/stat", p, &st); err != nil {
		return true, true
	}
	return st.Exists, st.Dir
}

func (s *Service) ListDirs(p string) ([]string, error) {
	var d []string
	return d, s.get("/v1/dirs", p, &d)
}

// MountOf returns the volume; with the service unreachable a mount without
// FSType, which QuotaStatus reports as install_host_service.
func (s *Service) MountOf(p string) (Mount, error) {
	var m struct{ Point, FSType, Status string }
	if err := s.get("/v1/mount", p, &m); err != nil {
		if err == fs.ErrNotExist {
			return Mount{}, err
		}
		return Mount{Point: drive(p)}, nil
	}
	return Mount{Point: config.HostPath(m.Point), FSType: m.FSType, Options: m.Status}, nil
}

func drive(p string) string {
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		return p[:3]
	}
	return p
}

func (s *Service) ProjectID(p string) (uint32, error) {
	var r struct{ ID uint32 }
	err := s.get("/v1/id", p, &r)
	return r.ID, err
}

func (s *Service) QuotaStatus(m Mount) (string, error) {
	if m.FSType == "" {
		return InstallHostService, nil
	}
	return m.Options, nil
}

func (s *Service) Usage(ctx context.Context, m Mount, minID uint32) (map[uint32]Usage, error) {
	var r map[string]Usage
	q := url.Values{"volume": {config.NativePath(m.Point)}, "min": {strconv.FormatUint(uint64(minID), 10)}}
	if err := s.call(ctx, s.hc, http.MethodGet, "/v1/usage", q, nil, &r); err != nil {
		return nil, err
	}
	out := make(map[uint32]Usage, len(r))
	for k, u := range r {
		id, err := strconv.ParseUint(k, 10, 32)
		if err != nil {
			return nil, err
		}
		out[uint32(id)] = u
	}
	return out, nil
}

// Walk assigns the tree to id at the service, which walks it; inherit, skip
// and the rate are Linux matters.
func (s *Service) Walk(ctx context.Context, root string, id uint32, _ bool, _ map[string]bool, _ int) error {
	var r struct{ OK bool }
	body := map[string]any{"path": config.NativePath(root), "id": id}
	return s.call(ctx, s.walk, http.MethodPost, "/v1/assign", url.Values{}, body, &r)
}

func (s *Service) ZFSVersions() (string, string) { return "", "" }

// Endpoints returns the HNS network ID per stats network key.
func (s *Service) Endpoints(ctx context.Context) (map[string]string, error) {
	var eps []struct{ ID, Network string }
	if err := s.call(ctx, s.hc, http.MethodGet, "/v1/endpoints", url.Values{}, nil, &eps); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(eps))
	for _, e := range eps {
		out[upper(e.ID)] = upper(e.Network)
	}
	return out, nil
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}
