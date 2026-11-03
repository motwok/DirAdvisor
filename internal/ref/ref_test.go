// SPDX-License-Identifier: AGPL-3.0-or-later

package ref

import "testing"

func TestParse(t *testing.T) {
	for in, want := range map[string]string{
		"nginx":                                  "docker.io/library/nginx:latest",
		"nginx:1.29":                             "docker.io/library/nginx:1.29",
		"library/nginx":                          "docker.io/library/nginx:latest",
		"bitnami/redis:7":                        "docker.io/bitnami/redis:7",
		"index.docker.io/library/nginx:1":        "docker.io/library/nginx:1",
		"docker.io/nginx":                        "docker.io/library/nginx:latest",
		"ghcr.io/motwok/imageadvisor":            "ghcr.io/motwok/imageadvisor:latest",
		"localhost/app:dev":                      "localhost/app:dev",
		"localhost:5000/app":                     "localhost:5000/app:latest",
		"reg.example:5000/a/b:1.2":               "reg.example:5000/a/b:1.2",
		"nginx@sha256:abc":                       "docker.io/library/nginx@sha256:abc",
		"nginx:1.29@sha256:abc":                  "docker.io/library/nginx:1.29@sha256:abc",
		"ia-reg:5000/sw:1@sha256:abc":            "ia-reg:5000/sw:1@sha256:abc",
		"mcr.microsoft.com/windows/nanoserver:x": "mcr.microsoft.com/windows/nanoserver:x",
		"MyProject/app":                          "MyProject/app:latest",
	} {
		if got := Parse(in).String(); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
	r := Parse("ghcr.io/a/b:1@sha256:d")
	if r.Domain != "ghcr.io" || r.Path != "a/b" || r.Tag != "1" || r.Digest != "sha256:d" || r.Repository() != "ghcr.io/a/b" {
		t.Fatalf("%+v", r)
	}
	if NormalizeHost("index.docker.io") != "docker.io" || NormalizeHost("ghcr.io") != "ghcr.io" {
		t.Fatal("NormalizeHost")
	}
}

func TestIsImageID(t *testing.T) {
	id := "sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"
	for s, want := range map[string]bool{
		id:             true,
		id[7:]:         true,
		"a7dbc319":     true,
		"sha256:a7dbc": true,
		"":             false,
		"sha256:":      false,
		"b7dbc319":     false,
		"nginx":        false,
		"A7DBC":        false,
	} {
		if IsImageID(s, id) != want {
			t.Errorf("%q: want %v", s, want)
		}
	}
	if IsImageID("a7dg", "sha256:a7dg") {
		t.Fatal("non-hex prefix")
	}
}

func TestRepoDigest(t *testing.T) {
	repo, d, ok := RepoDigest("nginx@sha256:aa")
	if !ok || repo != "docker.io/library/nginx" || d != "sha256:aa" {
		t.Fatal(repo, d, ok)
	}
	if _, _, ok := RepoDigest("nginx"); ok {
		t.Fatal("no digest")
	}
}
