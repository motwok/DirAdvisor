// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/motwok/HatchLens/internal/config"
	"github.com/motwok/HatchLens/internal/winhost"
)

// Service is the FS on Windows, backed by the HatchLens Host part: over its
// named pipe from a container, or in the same process when HatchLens runs as
// a Windows service on the host. A file counts for the innermost assigned
// directory there, as a file carries one project ID on Linux. Paths go out in
// Windows form.
type Service struct {
	r remote
}

// remote is the HatchLens Host API with Windows paths.
type remote interface {
	endpoints(ctx context.Context) ([]winhost.Endpoint, error)
	stat(path string) (winhost.Stat, error)
	dirs(path string) ([]string, error)
	mount(path string) (winhost.Mount, error)
	id(path string) (uint32, error)
	assign(ctx context.Context, path string, id uint32) error
	usage(ctx context.Context, volume string, min uint32) (map[uint32]winhost.Usage, error)
}

// NewService talks to the HatchLens Host service over its pipe.
func NewService(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *Service {
	t := &http.Transport{DialContext: dial}
	return &Service{r: &pipe{hc: &http.Client{Timeout: 30 * time.Second, Transport: t}, walk: &http.Client{Transport: t}}}
}

// NewLocal uses the HatchLens Host part in this process.
func NewLocal(b winhost.Backend) *Service { return &Service{r: local{b}} }

// Stat reports an unreachable service as an existing directory, so the entry
// goes on to MountOf and shows install_host_service instead of path_missing.
func (s *Service) Stat(p string) (bool, bool) {
	st, err := s.r.stat(config.NativePath(p))
	if err != nil {
		return true, true
	}
	return st.Exists, st.Dir
}

func (s *Service) ListDirs(p string) ([]string, error) { return s.r.dirs(config.NativePath(p)) }

// MountOf returns the volume; with the service unreachable a mount without
// FSType, which QuotaStatus reports as install_host_service.
func (s *Service) MountOf(p string) (Mount, error) {
	m, err := s.r.mount(config.NativePath(p))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Mount{}, fs.ErrNotExist
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

func (s *Service) ProjectID(p string) (uint32, error) { return s.r.id(config.NativePath(p)) }

func (s *Service) QuotaStatus(m Mount) (string, error) {
	if m.FSType == "" {
		return InstallHostService, nil
	}
	return m.Options, nil
}

func (s *Service) Usage(ctx context.Context, m Mount, minID uint32) (map[uint32]Usage, error) {
	u, err := s.r.usage(ctx, config.NativePath(m.Point), minID)
	if err != nil {
		return nil, err
	}
	out := make(map[uint32]Usage, len(u))
	for id, x := range u {
		out[id] = Usage{Bytes: x.Bytes, Inodes: x.Inodes}
	}
	return out, nil
}

// Walk assigns the tree to id at the service, which walks it; inherit, skip
// and the rate are Linux matters.
func (s *Service) Walk(ctx context.Context, root string, id uint32, _ bool, _ map[string]bool, _ int) error {
	return s.r.assign(ctx, config.NativePath(root), id)
}

func (s *Service) ZFSVersions() (string, string) { return "", "" }

// Endpoints returns the HNS network ID per stats network key.
func (s *Service) Endpoints(ctx context.Context) (map[string]string, error) {
	eps, err := s.r.endpoints(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(eps))
	for _, e := range eps {
		out[strings.ToUpper(e.ID)] = strings.ToUpper(e.Network)
	}
	return out, nil
}

// local calls the backend in this process.
type local struct{ b winhost.Backend }

func (l local) endpoints(context.Context) ([]winhost.Endpoint, error) { return l.b.Endpoints() }
func (l local) stat(p string) (winhost.Stat, error)                   { return l.b.Stat(p), nil }
func (l local) dirs(p string) ([]string, error)                       { return l.b.Dirs(p) }
func (l local) mount(p string) (winhost.Mount, error)                 { return l.b.Mount(p) }
func (l local) id(p string) (uint32, error)                           { return l.b.ID(p) }
func (l local) assign(ctx context.Context, p string, id uint32) error {
	return l.b.Assign(ctx, p, id)
}
func (l local) usage(_ context.Context, v string, min uint32) (map[uint32]winhost.Usage, error) {
	return l.b.Usage(v, min)
}

// unreachable hides the cause on purpose: a missing pipe is ERROR_FILE_NOT_FOUND,
// which must not read as a missing path.
type unreachable struct{ err error }

func (u unreachable) Error() string { return "host service unreachable: " + u.err.Error() }

// pipe calls the HatchLens Host service over HTTP on its named pipe.
type pipe struct{ hc, walk *http.Client }

func (p *pipe) call(ctx context.Context, c *http.Client, method, path string, q url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://hatchlens-host"+path+"?"+q.Encode(), rd)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return unreachable{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fs.ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("host service %s: %d %s", path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (p *pipe) get(path, arg string, out any) error {
	return p.call(context.Background(), p.hc, http.MethodGet, path, url.Values{"path": {arg}}, nil, out)
}

func (p *pipe) endpoints(ctx context.Context) ([]winhost.Endpoint, error) {
	var eps []winhost.Endpoint
	return eps, p.call(ctx, p.hc, http.MethodGet, "/v1/endpoints", url.Values{}, nil, &eps)
}

func (p *pipe) stat(path string) (winhost.Stat, error) {
	var st winhost.Stat
	return st, p.get("/v1/stat", path, &st)
}

func (p *pipe) dirs(path string) ([]string, error) {
	var d []string
	return d, p.get("/v1/dirs", path, &d)
}

func (p *pipe) mount(path string) (winhost.Mount, error) {
	var m winhost.Mount
	return m, p.get("/v1/mount", path, &m)
}

func (p *pipe) id(path string) (uint32, error) {
	var r struct{ ID uint32 }
	return r.ID, p.get("/v1/id", path, &r)
}

func (p *pipe) assign(ctx context.Context, path string, id uint32) error {
	var r struct{ OK bool }
	return p.call(ctx, p.walk, http.MethodPost, "/v1/assign", url.Values{}, map[string]any{"path": path, "id": id}, &r)
}

func (p *pipe) usage(ctx context.Context, volume string, min uint32) (map[uint32]winhost.Usage, error) {
	var r map[string]winhost.Usage
	q := url.Values{"volume": {volume}, "min": {strconv.FormatUint(uint64(min), 10)}}
	if err := p.call(ctx, p.hc, http.MethodGet, "/v1/usage", q, nil, &r); err != nil {
		return nil, err
	}
	out := make(map[uint32]winhost.Usage, len(r))
	for k, u := range r {
		id, err := strconv.ParseUint(k, 10, 32)
		if err != nil {
			return nil, err
		}
		out[uint32(id)] = u
	}
	return out, nil
}
