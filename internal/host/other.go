// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !linux

package host

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("only supported on Linux")

type other struct{}

// New returns a stub; DirAdvisor measures only on Linux.
func New(string) FS { return other{} }

func (other) Stat(string) (bool, bool)          { return false, false }
func (other) ListDirs(string) ([]string, error) { return nil, errUnsupported }
func (other) MountOf(string) (Mount, error)     { return Mount{}, errUnsupported }
func (other) ProjectID(string) (uint32, error)  { return 0, errUnsupported }
func (other) QuotaStatus(Mount) (string, error) { return FilesystemNotSupported, nil }
func (other) ZFSVersions() (string, string)     { return "", "" }
func (other) Usage(context.Context, Mount, uint32) (map[uint32]Usage, error) {
	return nil, errUnsupported
}
func (other) Walk(context.Context, string, uint32, bool, map[string]bool, int) error {
	return errUnsupported
}

func SelfContainerID() string { return "" }
