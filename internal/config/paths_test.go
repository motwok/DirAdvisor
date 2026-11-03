// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import "testing"

func TestPaths(t *testing.T) {
	for in, want := range map[string]string{
		`C:\ProgramData\docker\volumes\x\_data`: "/C:/ProgramData/docker/volumes/x/_data",
		`c:\`:                                   "/C:",
		`D:`:                                    "/D:",
		`d:/data/`:                              "/D:/data",
		"/srv/data":                             "/srv/data",
		"x":                                     "x",
		"1:/x":                                  "1:/x",
	} {
		if got := HostPath(in); got != want {
			t.Errorf("HostPath(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"/C:/ProgramData/docker": `C:\ProgramData\docker`,
		"/C:":                    `C:\`,
		"/srv/data":              "/srv/data",
		"/C:x":                   "/C:x",
		"/1:/x":                  "/1:/x",
		"/C":                     "/C",
	} {
		if got := NativePath(in); got != want {
			t.Errorf("NativePath(%q) = %q, want %q", in, got, want)
		}
	}
	if windows {
		if Host(`C:\x`) != "/C:/x" || Display("/C:/x") != `C:\x` {
			t.Fatal("Host/Display on Windows")
		}
	} else if Host(`C:\x`) != `C:\x` || Display("/C:/x") != "/C:/x" {
		t.Fatal("Host/Display elsewhere")
	}
	if !Excluded("/C:", "") || !Excluded("/C:/", "") || Excluded("/C:/data", "") {
		t.Fatal("a whole drive is excluded, a directory on it is not")
	}
}
