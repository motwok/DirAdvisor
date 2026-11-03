// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import "strings"

func driveLetter(b byte) bool { return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' }

// HostPath turns a Windows path into the form the core works with: forward
// slashes and a leading slash before the drive ("C:\x" -> "/C:/x"), so
// path.Clean and prefix checks work as for Linux paths. Others stay as they are.
func HostPath(p string) string {
	if len(p) < 2 || p[1] != ':' || !driveLetter(p[0]) {
		return p
	}
	rest := strings.ReplaceAll(p[2:], `\`, "/")
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return "/" + strings.ToUpper(p[:1]) + ":" + strings.TrimSuffix(rest, "/")
}

// NativePath is the reverse of HostPath ("/C:/x" -> `C:\x`, "/C:" -> `C:\`).
func NativePath(p string) string {
	if len(p) < 3 || p[0] != '/' || p[2] != ':' || !driveLetter(p[1]) || len(p) > 3 && p[3] != '/' {
		return p
	}
	rest := strings.ReplaceAll(p[3:], "/", `\`)
	if rest == "" {
		rest = `\`
	}
	return p[1:3] + rest
}

// Host is HostPath on Windows and the identity elsewhere.
func Host(p string) string {
	if windows {
		return HostPath(p)
	}
	return p
}

// Display is NativePath on Windows and the identity elsewhere; for labels and
// logs.
func Display(p string) string {
	if windows {
		return NativePath(p)
	}
	return p
}

// driveRoot reports whether p is a whole Windows drive in HostPath form.
func driveRoot(p string) bool {
	return len(p) == 3 && p[0] == '/' && p[2] == ':' && driveLetter(p[1])
}
