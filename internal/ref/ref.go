// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ref parses and normalizes image references the way Docker does.
package ref

import "strings"

const hub = "docker.io"

// Ref is a parsed image reference. Tag and Digest may be empty.
type Ref struct {
	Domain, Path, Tag, Digest string
}

// Parse splits an image reference; it does not validate it.
func Parse(s string) Ref {
	var r Ref
	if i := strings.Index(s, "@"); i >= 0 {
		s, r.Digest = s[:i], s[i+1:]
	}
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		s, r.Tag = s[:i], s[i+1:]
	}
	r.Domain, r.Path = hub, s
	if i := strings.Index(s, "/"); i >= 0 {
		// Docker: a first component with ".", ":", "localhost" or an uppercase letter is the registry.
		if first := s[:i]; strings.ContainsAny(first, ".:") || first == "localhost" || first != strings.ToLower(first) {
			r.Domain, r.Path = first, s[i+1:]
		}
	}
	r.Domain = NormalizeHost(r.Domain)
	if r.Domain == hub && !strings.Contains(r.Path, "/") {
		r.Path = "library/" + r.Path
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r
}

// NormalizeHost maps the alias of Docker Hub to docker.io.
func NormalizeHost(h string) string {
	if h == "index.docker.io" {
		return hub
	}
	return h
}

// Repository is the normalized repository without tag and digest.
func (r Ref) Repository() string { return r.Domain + "/" + r.Path }

// String is the normalized reference.
func (r Ref) String() string {
	s := r.Repository()
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// IsImageID reports whether s is imageID or a prefix of it, with or without "sha256:".
func IsImageID(s, imageID string) bool {
	s = strings.TrimPrefix(s, "sha256:")
	id := strings.TrimPrefix(imageID, "sha256:")
	if s == "" || !strings.HasPrefix(id, s) {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// RepoDigest splits a RepoDigests entry ("repo@sha256:...") into normalized
// repository and digest.
func RepoDigest(s string) (repo, digest string, ok bool) {
	i := strings.LastIndex(s, "@")
	if i < 0 {
		return "", "", false
	}
	return Parse(s[:i]).Repository(), s[i+1:], true
}
