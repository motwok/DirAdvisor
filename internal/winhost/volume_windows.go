// SPDX-License-Identifier: AGPL-3.0-or-later

package winhost

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sync"

	"golang.org/x/sys/windows"
)

type node struct {
	parent uint64
	dir    bool
	alloc  int64
}

type root struct {
	path string
	id   uint32
}

// volume keeps the trees below the assigned roots of one NTFS volume current
// from the USN journal. A file counts for its innermost root, like a file
// carries one project ID on Linux.
type volume struct {
	mu      sync.Mutex
	point   string
	h, hint windows.Handle
	j       journal
	roots   map[uint64]root
	nodes   map[uint64]*node
	open    map[uint64]bool // files with a change not closed yet
}

// Replaceable in tests.
var rewalkHook = func() {}

func openVolume(point string) (*volume, error) {
	dev, err := volumeName(point)
	if err != nil {
		return nil, err
	}
	h, err := open(dev, windows.GENERIC_READ, 0)
	if err != nil {
		return nil, err
	}
	j, err := queryJournal(h)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	hint, err := open(point, windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return &volume{point: point, h: h, hint: hint, j: j, roots: map[uint64]root{}, nodes: map[uint64]*node{},
		open: map[uint64]bool{}}, nil
}

// walk adds the tree below dir to nodes. Reparse points are not entered;
// unreadable subdirectories count as empty.
func walk(dir string, frn uint64, nodes map[uint64]*node) error {
	es, err := list(dir)
	if err != nil {
		return err
	}
	if nodes[frn] == nil {
		nodes[frn] = &node{dir: true}
	}
	for _, e := range es {
		nd := &node{parent: frn, dir: e.attrs&attrDirectory != 0}
		if !nd.dir {
			nd.alloc = e.alloc
		}
		nodes[e.frn] = nd
		if nd.dir && e.attrs&attrReparse == 0 {
			walk(filepath.Join(dir, e.name), e.frn, nodes)
		}
	}
	return nil
}

func (v *volume) id(path string) (uint32, error) {
	frn, err := fileID(path)
	if err != nil {
		return 0, notExist(err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.roots[frn].id, nil
}

func (v *volume) assign(ctx context.Context, path string, id uint32) error {
	frn, err := fileID(path)
	if err != nil {
		return notExist(err)
	}
	v.mu.Lock()
	known := v.nodes[frn] != nil
	if id == 0 {
		delete(v.roots, frn)
	}
	v.mu.Unlock()
	if id == 0 {
		return nil
	}
	if !known {
		add := map[uint64]*node{}
		if err := walk(path, frn, add); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		v.mu.Lock()
		for k, nd := range add {
			if v.nodes[k] == nil {
				v.nodes[k] = nd
			}
		}
		v.mu.Unlock()
	}
	v.mu.Lock()
	v.roots[frn] = root{path: path, id: id}
	v.mu.Unlock()
	return nil
}

// rewalk rebuilds the nodes of all roots, after a lost journal and once an
// hour against drift (a file open since before the service started changes
// without new journal records). Roots assigned meanwhile are walked too; a
// root whose directory is gone is dropped.
func (v *volume) rewalk() {
	nodes, seen := map[uint64]*node{}, map[uint64]bool{}
	for {
		v.mu.Lock()
		todo := map[uint64]root{}
		for frn, r := range v.roots {
			if !seen[frn] {
				todo[frn] = r
			}
		}
		if len(todo) == 0 {
			v.nodes, v.open = nodes, map[uint64]bool{}
			v.mu.Unlock()
			return
		}
		v.mu.Unlock()
		rewalkHook()
		for frn, r := range todo {
			seen[frn] = true
			if err := walk(r.path, frn, nodes); errors.Is(notExist(err), fs.ErrNotExist) {
				v.mu.Lock()
				delete(v.roots, frn)
				v.mu.Unlock()
			}
		}
	}
}

// poll applies the journal since the last call and re-measures every file it
// reported plus every file with an unclosed change.
func (v *volume) poll() error {
	recs, next, err := readJournal(v.h, v.j.id, v.j.next)
	if err != nil {
		j, qerr := queryJournal(v.h)
		if qerr != nil {
			return qerr
		}
		v.j = j
		v.rewalk()
		return nil
	}
	v.j.next = next
	v.mu.Lock()
	dirty := map[uint64]bool{}
	var movedIn []uint64
	for _, r := range recs {
		if r.reason&reasonFileDelete != 0 {
			delete(v.nodes, r.frn)
			delete(v.open, r.frn)
			delete(v.roots, r.frn)
			continue
		}
		nd, parentKnown := v.nodes[r.frn], v.nodes[r.parent] != nil
		switch {
		case nd == nil && !parentKnown:
			continue
		case nd == nil:
			nd = &node{parent: r.parent, dir: r.attrs&attrDirectory != 0}
			v.nodes[r.frn] = nd
			if nd.dir && r.reason&reasonRenameNew != 0 {
				movedIn = append(movedIn, r.frn)
			}
		case r.reason&reasonRenameNew != 0 && !parentKnown:
			delete(v.nodes, r.frn)
			delete(v.open, r.frn)
			continue
		case r.reason&reasonRenameNew != 0:
			nd.parent = r.parent
		}
		if r.reason&reasonData != 0 {
			dirty[r.frn] = true
			if r.reason&reasonClose == 0 {
				v.open[r.frn] = true
			}
		}
		if r.reason&reasonClose != 0 {
			delete(v.open, r.frn)
		}
	}
	for f := range v.open {
		dirty[f] = true
	}
	v.mu.Unlock()
	measured := map[uint64]int64{}
	for f := range dirty {
		if alloc, ok := v.measure(f); ok {
			measured[f] = alloc
		}
	}
	for _, f := range movedIn {
		if p, err := v.pathOf(f); err == nil {
			add := map[uint64]*node{}
			walk(p, f, add)
			v.mu.Lock()
			for k, nd := range add {
				if k != f {
					v.nodes[k] = nd
				}
			}
			v.mu.Unlock()
		}
	}
	v.mu.Lock()
	for f, alloc := range measured {
		if nd := v.nodes[f]; nd != nil {
			nd.alloc = alloc
		}
	}
	v.mu.Unlock()
	return nil
}

func (v *volume) measure(frn uint64) (int64, bool) {
	h, err := openByID(v.hint, frn)
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	alloc, _, err := sizes(h)
	return alloc, err == nil
}

func (v *volume) pathOf(frn uint64) (string, error) {
	h, err := openByID(v.hint, frn)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := finalPathName(h, &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// usage sums allocated bytes and entries per id, each node for its innermost
// root.
func (v *volume) usage(min uint32) map[uint32]Usage {
	v.mu.Lock()
	defer v.mu.Unlock()
	owner := map[uint64]uint32{}
	var resolve func(frn uint64, depth int) uint32
	resolve = func(frn uint64, depth int) uint32 {
		if r, ok := v.roots[frn]; ok {
			return r.id
		}
		if id, ok := owner[frn]; ok {
			return id
		}
		nd := v.nodes[frn]
		if nd == nil || depth > 4096 {
			return 0
		}
		id := resolve(nd.parent, depth+1)
		owner[frn] = id
		return id
	}
	out := map[uint32]Usage{}
	for frn, nd := range v.nodes {
		id := resolve(frn, 0)
		if id == 0 || id < min {
			continue
		}
		u := out[id]
		u.Bytes += uint64(nd.alloc)
		u.Inodes++
		out[id] = u
	}
	return out
}

func notExist(err error) error {
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return errors.Join(fs.ErrNotExist, err)
	}
	return err
}
