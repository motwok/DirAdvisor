// SPDX-License-Identifier: AGPL-3.0-or-later

package host

import (
	"context"

	"github.com/motwok/HatchLens/internal/config"
)

// Split sends paths on Windows drives (/C:/x) to Remote, the HatchLens Host
// service, and the rest to Local: a Linux container under Docker Desktop.
type Split struct{ Local, Remote FS }

func (s Split) pick(p string) FS {
	if config.Drive(p) {
		return s.Remote
	}
	return s.Local
}

func (s Split) Stat(p string) (bool, bool)             { return s.pick(p).Stat(p) }
func (s Split) ListDirs(p string) ([]string, error)    { return s.pick(p).ListDirs(p) }
func (s Split) MountOf(p string) (Mount, error)        { return s.pick(p).MountOf(p) }
func (s Split) ProjectID(p string) (uint32, error)     { return s.pick(p).ProjectID(p) }
func (s Split) QuotaStatus(m Mount) (string, error)    { return s.pick(m.Point).QuotaStatus(m) }
func (s Split) ZFSVersions() (module, userland string) { return s.Local.ZFSVersions() }

func (s Split) Usage(ctx context.Context, m Mount, minID uint32) (map[uint32]Usage, error) {
	return s.pick(m.Point).Usage(ctx, m, minID)
}

func (s Split) Walk(ctx context.Context, root string, id uint32, inherit bool, skip map[string]bool, perSecond int) error {
	return s.pick(root).Walk(ctx, root, id, inherit, skip, perSecond)
}
