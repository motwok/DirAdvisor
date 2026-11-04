// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package host

import (
	"context"
	"os"
	"testing"
)

func TestStub(t *testing.T) {
	fs := New("/")
	ctx := context.Background()
	if e, d := fs.Stat("/"); e || d {
		t.Fatal("Stat")
	}
	if _, err := fs.ListDirs("/"); err == nil {
		t.Fatal("ListDirs")
	}
	if _, err := fs.MountOf("/"); err == nil {
		t.Fatal("MountOf")
	}
	if _, err := fs.ProjectID("/"); err == nil {
		t.Fatal("ProjectID")
	}
	if s, err := fs.QuotaStatus(Mount{}); s != FilesystemNotSupported || err != nil {
		t.Fatal("QuotaStatus")
	}
	if m, u := fs.ZFSVersions(); m != "" || u != "" {
		t.Fatal("ZFSVersions")
	}
	if _, err := fs.Usage(ctx, Mount{}, 1); err == nil {
		t.Fatal("Usage")
	}
	if err := fs.Walk(ctx, "/", 1, false, nil, 0); err == nil {
		t.Fatal("Walk")
	}
	if h, _ := os.Hostname(); SelfContainerID() != h {
		t.Fatal("SelfContainerID")
	}
}
