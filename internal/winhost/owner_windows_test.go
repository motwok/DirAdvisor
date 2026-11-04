// SPDX-License-Identifier: AGPL-3.0-or-later

package winhost

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// trustOwner makes temp dirs of a non-elevated test run pass CheckOwner.
func trustOwner(t *testing.T) {
	saved := fileOwner
	t.Cleanup(func() { fileOwner = saved })
	fileOwner = func(string) (*windows.SID, error) {
		return windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	}
}

func TestCheckOwner(t *testing.T) {
	dir := t.TempDir()
	if _, err := fileOwner(dir); err != nil {
		t.Fatal(err)
	}
	if err := CheckOwner(filepath.Join(dir, "missing")); err != nil {
		t.Fatal(err)
	}
	if err := CheckOwner(filepath.Join(dir, "missing", "sub")); err != nil {
		t.Fatal(err)
	}
	if err := CheckOwner("bad\x00name"); err == nil {
		t.Fatal("bad name")
	}
	saved := fileOwner
	defer func() { fileOwner = saved }()
	for _, k := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		fileOwner = func(string) (*windows.SID, error) { return windows.CreateWellKnownSid(k) }
		if err := CheckOwner(dir); err != nil {
			t.Fatal(err)
		}
	}
	fileOwner = func(string) (*windows.SID, error) { return windows.CreateWellKnownSid(windows.WinWorldSid) }
	if err := CheckOwner(dir); err == nil {
		t.Fatal("owned by Everyone")
	}
}
