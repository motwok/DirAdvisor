// SPDX-License-Identifier: AGPL-3.0-or-later

package winhost

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type apiFake struct{ fail bool }

func (f apiFake) err() error {
	if f.fail {
		return errors.New("broken")
	}
	return nil
}
func (f apiFake) Endpoints() ([]Endpoint, error)               { return []Endpoint{{ID: "e", Network: "n"}}, f.err() }
func (f apiFake) Stat(string) Stat                             { return Stat{Exists: true} }
func (f apiFake) Dirs(string) ([]string, error)                { return nil, fs.ErrNotExist }
func (f apiFake) Mount(string) (Mount, error)                  { return Mount{Point: `C:\`}, f.err() }
func (f apiFake) ID(string) (uint32, error)                    { return 3, f.err() }
func (f apiFake) Assign(context.Context, string, uint32) error { return f.err() }
func (f apiFake) Usage(string, uint32) (map[uint32]Usage, error) {
	return map[uint32]Usage{5: {Bytes: 1, Inodes: 2}}, f.err()
}

func call(t *testing.T, h http.Handler, method, url, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, url, strings.NewReader(body)))
	b, _ := io.ReadAll(w.Body)
	return w.Code, string(b)
}

func TestHandler(t *testing.T) {
	h := Handler(apiFake{}, "1.2.3")
	for _, c := range []struct {
		method, url, body string
		code              int
		want              string
	}{
		{"GET", "/v1/version", "", 200, `"1.2.3"`},
		{"GET", "/v1/endpoints", "", 200, `"network":"n"`},
		{"GET", "/v1/stat?path=x", "", 200, `"exists":true`},
		{"GET", "/v1/dirs?path=x", "", 404, ""},
		{"GET", "/v1/mount?path=x", "", 200, `"point":"C:\\`},
		{"GET", "/v1/id?path=x", "", 200, `"id":3`},
		{"POST", "/v1/assign", `{"path":"x","id":4}`, 200, `"ok":true`},
		{"POST", "/v1/assign", `{`, 400, ""},
		{"GET", "/v1/usage?volume=C:&min=1", "", 200, `"5":{"bytes":1,"inodes":2}`},
		{"POST", "/v1/stat", "", 405, ""},
	} {
		code, body := call(t, h, c.method, c.url, c.body)
		if code != c.code || !strings.Contains(body, c.want) {
			t.Errorf("%s %s: %d %s", c.method, c.url, code, body)
		}
	}
	bad := Handler(apiFake{fail: true}, "x")
	for _, url := range []string{"/v1/endpoints", "/v1/mount", "/v1/id", "/v1/usage"} {
		if code, _ := call(t, bad, "GET", url, ""); code != 500 {
			t.Errorf("%s: %d", url, code)
		}
	}
}
