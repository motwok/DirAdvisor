// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

type recFS struct{ calls []string }

func (r *recFS) Stat(p string) (bool, bool) { r.calls = append(r.calls, "Stat "+p); return true, true }
func (r *recFS) ListDirs(p string) ([]string, error) {
	r.calls = append(r.calls, "ListDirs "+p)
	return nil, nil
}
func (r *recFS) MountOf(p string) (Mount, error) {
	r.calls = append(r.calls, "MountOf "+p)
	return Mount{}, nil
}
func (r *recFS) ProjectID(p string) (uint32, error) {
	r.calls = append(r.calls, "ProjectID "+p)
	return 0, nil
}
func (r *recFS) QuotaStatus(m Mount) (string, error) {
	r.calls = append(r.calls, "QuotaStatus "+m.Point)
	return "", nil
}
func (r *recFS) Usage(_ context.Context, m Mount, _ uint32) (map[uint32]Usage, error) {
	r.calls = append(r.calls, "Usage "+m.Point)
	return nil, nil
}
func (r *recFS) Walk(_ context.Context, root string, _ uint32, _ bool, _ map[string]bool, _ int) error {
	r.calls = append(r.calls, "Walk "+root)
	return nil
}
func (r *recFS) ZFSVersions() (string, string) { r.calls = append(r.calls, "ZFS"); return "", "" }

func TestSplit(t *testing.T) {
	l, r := &recFS{}, &recFS{}
	s := Split{Local: l, Remote: r}
	ctx := context.Background()
	for _, p := range []string{"/var/lib/docker/volumes/a/_data", "/C:/data"} {
		s.Stat(p)
		s.ListDirs(p)
		s.MountOf(p)
		s.ProjectID(p)
		s.QuotaStatus(Mount{Point: p})
		s.Usage(ctx, Mount{Point: p}, 1)
		s.Walk(ctx, p, 1, true, nil, 0)
	}
	s.ZFSVersions()
	if len(l.calls) != 8 || len(r.calls) != 7 || r.calls[0] != "Stat /C:/data" || l.calls[7] != "ZFS" {
		t.Fatalf("local %v remote %v", l.calls, r.calls)
	}
}

func TestServiceToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Write([]byte(`{"Exists":true,"Dir":true}`))
	}))
	defer srv.Close()
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}
	if ok, _ := NewService("tok", dial).Stat("/C:/x"); !ok || got != "Bearer tok" {
		t.Fatalf("%v %q", ok, got)
	}
	if NewService("", dial).Stat("/C:/x"); got != "" {
		t.Fatalf("no token: %q", got)
	}
}
