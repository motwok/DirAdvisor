// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"path"
	"sort"
	"strconv"
	"strings"
)

// StaticPrefix is the part of a path glob before the first segment with "*".
func StaticPrefix(p string) string {
	segs := strings.Split(strings.TrimPrefix(path.Clean(p), "/"), "/")
	out := "/"
	for _, s := range segs {
		if strings.Contains(s, "*") {
			break
		}
		out = path.Join(out, s)
	}
	return out
}

// MatchName matches a name glob; "*" matches any characters.
func MatchName(pattern, name string) bool {
	_, ok := matchSeg(pattern, name)
	return ok
}

func matchSeg(pat, s string) ([]string, bool) {
	if pat == "" {
		return nil, s == ""
	}
	if pat[0] == '*' {
		for k := 0; k <= len(s); k++ {
			if caps, ok := matchSeg(pat[1:], s[k:]); ok {
				return append([]string{s[:k]}, caps...), true
			}
		}
		return nil, false
	}
	if s == "" || pat[0] != s[0] {
		return nil, false
	}
	return matchSeg(pat[1:], s[1:])
}

// MatchPath matches a path glob; "*" stays within one path segment.
func MatchPath(pattern, p string) bool {
	ps := strings.Split(path.Clean(pattern), "/")
	ss := strings.Split(path.Clean(p), "/")
	if len(ps) != len(ss) {
		return false
	}
	for i := range ps {
		if _, ok := matchSeg(ps[i], ss[i]); !ok {
			return false
		}
	}
	return true
}

type GlobMatch struct {
	Path     string
	Captures []string
}

// ListDirs returns the names of the subdirectories (no symlinks) of a host path.
type ListDirs func(dir string) ([]string, error)

// IsDir reports whether a host path is a directory (not a symlink).
type IsDir func(p string) bool

// ExpandGlob expands a path glob. parentMissing is set when its static prefix
// does not exist.
func ExpandGlob(pattern string, list ListDirs, isDir IsDir) (matches []GlobMatch, parentMissing bool, err error) {
	prefix := StaticPrefix(pattern)
	if !isDir(prefix) {
		return nil, true, nil
	}
	all := strings.Split(strings.TrimPrefix(path.Clean(pattern), "/"), "/")
	start := 0
	if prefix != "/" {
		start = len(strings.Split(strings.TrimPrefix(prefix, "/"), "/"))
	}
	var walk func(dir string, idx int, caps []string) error
	walk = func(dir string, idx int, caps []string) error {
		if idx == len(all) {
			matches = append(matches, GlobMatch{Path: dir, Captures: caps})
			return nil
		}
		seg := all[idx]
		if !strings.Contains(seg, "*") {
			next := path.Join(dir, seg)
			if isDir(next) {
				return walk(next, idx+1, caps)
			}
			return nil
		}
		names, err := list(dir)
		if err != nil {
			return err
		}
		sort.Strings(names)
		for _, n := range names {
			if c, ok := matchSeg(seg, n); ok {
				nc := append(append([]string{}, caps...), c...)
				if err := walk(path.Join(dir, n), idx+1, nc); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err = walk(prefix, start, nil)
	return matches, false, err
}

// SubstCaptures replaces $1, $2, ... by the glob captures.
func SubstCaptures(v string, caps []string) string {
	if !strings.Contains(v, "$") {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); {
		if v[i] == '$' && i+1 < len(v) && v[i+1] >= '0' && v[i+1] <= '9' {
			j := i + 1
			for j < len(v) && v[j] >= '0' && v[j] <= '9' {
				j++
			}
			n, _ := strconv.Atoi(v[i+1 : j])
			if n >= 1 && n <= len(caps) {
				b.WriteString(caps[n-1])
			}
			i = j
			continue
		}
		b.WriteByte(v[i])
		i++
	}
	return b.String()
}
