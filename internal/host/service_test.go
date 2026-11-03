// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motwok/HatchLens/internal/winhost"
)

type fakeBackend struct{ assigned map[string]uint32 }

func (f *fakeBackend) Endpoints() ([]winhost.Endpoint, error) {
	return []winhost.Endpoint{{ID: "ab-1", Network: "net-x"}}, nil
}
func (f *fakeBackend) Stat(p string) winhost.Stat {
	return winhost.Stat{Exists: p == `C:\data`, Dir: true}
}
func (f *fakeBackend) Dirs(p string) ([]string, error) {
	if p == `C:\none` {
		return nil, fs.ErrNotExist
	}
	return []string{"a", "b"}, nil
}
func (f *fakeBackend) Mount(p string) (winhost.Mount, error) {
	if p == `C:\none` {
		return winhost.Mount{}, fs.ErrNotExist
	}
	return winhost.Mount{Point: `C:\`, FSType: "NTFS", Status: "enable_usn_journal"}, nil
}
func (f *fakeBackend) ID(p string) (uint32, error) { return f.assigned[p], nil }
func (f *fakeBackend) Assign(_ context.Context, p string, id uint32) error {
	if p == `C:\bad` {
		return errors.New("access denied")
	}
	f.assigned[p] = id
	return nil
}
func (f *fakeBackend) Usage(volume string, min uint32) (map[uint32]winhost.Usage, error) {
	if volume != `C:\` {
		return nil, errors.New("unknown volume")
	}
	return map[uint32]winhost.Usage{min: {Bytes: 4096, Inodes: 3}}, nil
}

func newTestService(t *testing.T) (*Service, *fakeBackend) {
	b := &fakeBackend{assigned: map[string]uint32{}}
	srv := httptest.NewServer(winhost.Handler(b, "test"))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return NewService(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}), b
}

func TestService(t *testing.T) {
	s, b := newTestService(t)
	if e, d := s.Stat("/C:/data"); !e || !d {
		t.Fatal("stat")
	}
	if e, _ := s.Stat("/C:/other"); e {
		t.Fatal("stat missing")
	}
	if d, err := s.ListDirs("/C:/data"); err != nil || len(d) != 2 {
		t.Fatalf("dirs %v %v", d, err)
	}
	if _, err := s.ListDirs("/C:/none"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dirs missing: %v", err)
	}
	m, err := s.MountOf("/C:/data")
	if err != nil || m.Point != "/C:" || m.FSType != "NTFS" {
		t.Fatalf("mount %+v %v", m, err)
	}
	if r, _ := s.QuotaStatus(m); r != EnableUSNJournal {
		t.Fatalf("status %q", r)
	}
	if _, err := s.MountOf("/C:/none"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("mount missing: %v", err)
	}
	if err := s.Walk(context.Background(), "/C:/data", 7, true, nil, 0); err != nil || b.assigned[`C:\data`] != 7 {
		t.Fatalf("walk %v %v", err, b.assigned)
	}
	if err := s.Walk(context.Background(), "/C:/bad", 7, true, nil, 0); err == nil {
		t.Fatal("assign error must come through")
	}
	if id, err := s.ProjectID("/C:/data"); err != nil || id != 7 {
		t.Fatalf("id %d %v", id, err)
	}
	u, err := s.Usage(context.Background(), m, 7)
	if err != nil || u[7] != (Usage{Bytes: 4096, Inodes: 3}) {
		t.Fatalf("usage %v %v", u, err)
	}
	if _, err := s.Usage(context.Background(), Mount{Point: "/D:"}, 1); err == nil {
		t.Fatal("usage error")
	}
	//lint:ignore SA1012 a nil context makes building the request fail
	if _, err := s.Usage(nil, m, 1); err == nil { //nolint:staticcheck
		t.Fatal("nil context")
	}
	eps, err := s.Endpoints(context.Background())
	if err != nil || eps["AB-1"] != "NET-X" {
		t.Fatalf("endpoints %v %v", eps, err)
	}
	if a, b := s.ZFSVersions(); a != "" || b != "" {
		t.Fatal("zfs")
	}
}

func TestServiceUnreachable(t *testing.T) {
	s := NewService(func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no pipe") })
	if e, d := s.Stat("/C:/data"); !e || !d {
		t.Fatal("an unreachable service must not look like a missing path")
	}
	m, err := s.MountOf("/C:/data")
	if err != nil || m.Point != "/C:" || m.FSType != "" {
		t.Fatalf("mount %+v %v", m, err)
	}
	if r, _ := s.QuotaStatus(m); r != InstallHostService {
		t.Fatalf("status %q", r)
	}
	missing := NewService(func(context.Context, string, string) (net.Conn, error) { return nil, fs.ErrNotExist })
	if m, err := missing.MountOf("/C:/data"); err != nil || m.Point != "/C:" {
		t.Fatalf("a missing pipe is an unreachable service, not a missing path: %+v %v", m, err)
	}
	if m, _ := s.MountOf("relative"); m.Point != "relative" {
		t.Fatal("drive of a path without drive")
	}
	if _, err := s.Endpoints(context.Background()); err == nil || !strings.Contains(err.Error(), "host service unreachable: no pipe") {
		t.Fatalf("endpoints: %v", err)
	}
	if !AdminAction(InstallHostService) || !AdminAction(EnableUSNJournal) {
		t.Fatal("both are admin actions")
	}
}

func TestServiceBadUsageKey(t *testing.T) {
	srv := httptest.NewServer(httpHandlerFunc(`{"x":{"bytes":1}}`))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	s := NewService(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	})
	if _, err := s.Usage(context.Background(), Mount{Point: "/C:"}, 1); err == nil {
		t.Fatal("a key that is no ID is an error")
	}
}

type downBackend struct{ *fakeBackend }

func (downBackend) Mount(string) (winhost.Mount, error) { return winhost.Mount{}, errors.New("broken") }

func TestLocal(t *testing.T) {
	b := &fakeBackend{assigned: map[string]uint32{}}
	s := NewLocal(b)
	if e, d := s.Stat("/C:/data"); !e || !d {
		t.Fatal("stat")
	}
	if d, err := s.ListDirs("/C:/data"); err != nil || len(d) != 2 {
		t.Fatalf("dirs %v %v", d, err)
	}
	m, err := s.MountOf("/C:/data")
	if err != nil || m.Point != "/C:" || m.Options != EnableUSNJournal {
		t.Fatalf("mount %+v %v", m, err)
	}
	if _, err := s.MountOf("/C:/none"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("mount missing: %v", err)
	}
	if err := s.Walk(context.Background(), "/C:/data", 9, false, nil, 0); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.ProjectID("/C:/data"); id != 9 {
		t.Fatalf("id %d", id)
	}
	if u, err := s.Usage(context.Background(), m, 9); err != nil || u[9].Inodes != 3 {
		t.Fatalf("usage %v %v", u, err)
	}
	if eps, err := s.Endpoints(context.Background()); err != nil || eps["AB-1"] != "NET-X" {
		t.Fatalf("endpoints %v %v", eps, err)
	}
	down := NewLocal(downBackend{b})
	if m, err := down.MountOf("/C:/data"); err != nil || m.FSType != "" {
		t.Fatalf("broken backend: %+v %v", m, err)
	}
}
