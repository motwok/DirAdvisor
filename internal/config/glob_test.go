// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"reflect"
	"testing"
)

func TestStaticPrefix(t *testing.T) {
	cases := map[string]string{"/home/*": "/home", "/*": "/", "/a/b/c": "/a/b/c", "/a/x*/c": "/a"}
	for in, want := range cases {
		if got := StaticPrefix(in); got != want {
			t.Errorf("StaticPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchName(t *testing.T) {
	yes := [][2]string{{"buildx_*", "buildx_cache"}, {"*", ""}, {"a*b*c", "aXXbYc"}, {"exact", "exact"}, {"*x", "a/x"}}
	no := [][2]string{{"buildx_*", "cache"}, {"a*b", "ac"}, {"exact", "exactly"}}
	for _, c := range yes {
		if !MatchName(c[0], c[1]) {
			t.Errorf("%q should match %q", c[0], c[1])
		}
	}
	for _, c := range no {
		if MatchName(c[0], c[1]) {
			t.Errorf("%q should not match %q", c[0], c[1])
		}
	}
}

func TestMatchPath(t *testing.T) {
	if !MatchPath("/srv/*/data", "/srv/a/data") || MatchPath("/srv/*", "/srv/a/b") || MatchPath("/srv/*", "/opt/a") {
		t.Fatal("MatchPath")
	}
}

func TestExpandGlob(t *testing.T) {
	tree := map[string][]string{
		"/":             {"home", "srv"},
		"/home":         {"alice", "bob"},
		"/srv":          {"a-1", "a-2", "b-1"},
		"/srv/a-1":      {"data"},
		"/srv/a-2":      {},
		"/srv/b-1":      {"data"},
		"/home/bob":     {},
		"/home/alice":   {},
		"/srv/a-1/data": {},
		"/srv/b-1/data": {},
	}
	list := func(d string) ([]string, error) { return tree[d], nil }
	isDir := func(p string) bool { _, ok := tree[p]; return ok }

	m, missing, err := ExpandGlob("/home/*", list, isDir)
	if err != nil || missing || len(m) != 2 || m[0].Path != "/home/alice" || m[0].Captures[0] != "alice" {
		t.Fatalf("home: %+v %v %v", m, missing, err)
	}
	m, _, _ = ExpandGlob("/srv/a-*/data", list, isDir)
	if len(m) != 1 || m[0].Path != "/srv/a-1/data" || !reflect.DeepEqual(m[0].Captures, []string{"1"}) {
		t.Fatalf("srv: %+v", m)
	}
	m, _, _ = ExpandGlob("/*", list, isDir)
	if len(m) != 2 {
		t.Fatalf("root: %+v", m)
	}
	if _, missing, _ := ExpandGlob("/nope/*", list, isDir); !missing {
		t.Fatal("parent should be missing")
	}
	fail := func(string) ([]string, error) { return nil, errors.New("boom") }
	if _, _, err := ExpandGlob("/home/*", fail, isDir); err == nil {
		t.Fatal("expected list error")
	}
	deep := func(d string) ([]string, error) {
		if d == "/srv/a-1" {
			return nil, errors.New("boom")
		}
		return tree[d], nil
	}
	if _, _, err := ExpandGlob("/srv/*/*", deep, isDir); err == nil {
		t.Fatal("expected nested list error")
	}
}

func TestSubstCaptures(t *testing.T) {
	caps := []string{"alice", "x"}
	cases := map[string]string{"$1": "alice", "u-$1-$2": "u-alice-x", "$3": "", "none": "none", "$": "$", "a$b": "a$b"}
	for in, want := range cases {
		if got := SubstCaptures(in, caps); got != want {
			t.Errorf("SubstCaptures(%q) = %q, want %q", in, got, want)
		}
	}
}
