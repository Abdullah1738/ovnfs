package export

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/overlay"
)

// recall tells the protocol layer that clients' cached copies of the named
// export paths are stale. Paths no client was ever handed are skipped.
func (n *nodeFS) recall(paths ...string) {
	fn := n.onRecall.Load()
	if fn == nil {
		return
	}
	for _, p := range paths {
		if id, ok := n.nodes.lookup(overlay.Clean(p)); ok {
			(*fn)(id)
		}
	}
}

// OpenFile recalls cached copies before any open that may change the file.
func (n *nodeFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&routerWriteFlags != 0 {
		n.recall(name)
	}
	return n.Filesystem.OpenFile(name, flag, perm)
}

// Create truncates through OpenFile so the recall is not bypassed.
func (n *nodeFS) Create(name string) (billy.File, error) {
	return n.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

// Chmod changes permissions after recalling cached attributes.
func (n *nodeFS) Chmod(name string, mode os.FileMode) error {
	n.recall(name)
	return n.Change.Chmod(name, mode)
}

// Lchown changes a link's ownership after recalling cached attributes.
func (n *nodeFS) Lchown(name string, uid, gid int) error {
	n.recall(name)
	return n.Change.Lchown(name, uid, gid)
}

// Chown changes ownership after recalling cached attributes.
func (n *nodeFS) Chown(name string, uid, gid int) error {
	n.recall(name)
	return n.Change.Chown(name, uid, gid)
}

// Chtimes changes times after recalling cached attributes.
func (n *nodeFS) Chtimes(name string, atime, mtime time.Time) error {
	n.recall(name)
	return n.Change.Chtimes(name, atime, mtime)
}

// OnRecall registers the frontend's cache invalidation hook for stable node ids;
// daemon changes and other filesystem consumers invoke fn, which must not block.
func (e *Export) OnRecall(fn func(id uint64)) { e.fs.onRecall.Store(&fn) }

// Invalidate recalls clients' cached copies of checkout paths the daemon
// changed behind the file protocols (pull, reattach, folder edits; nil paths
// cover the whole checkout). A path that no longer exists retires its node,
// so old handles report stale instead of reaching a later file of that name.
func (e *Export) Invalidate(checkout string, paths []string) {
	var targets []nodeRef
	if paths == nil {
		targets = e.fs.nodes.subtree(checkout)
	} else {
		for _, p := range paths {
			full := path.Join(checkout, p)
			if id, ok := e.fs.nodes.lookup(full); ok {
				targets = append(targets, nodeRef{id: id, path: full})
			}
		}
	}
	if fn := e.fs.onRecall.Load(); fn != nil {
		for _, t := range targets {
			(*fn)(t.id)
		}
	}
	var gone []string
	for _, t := range targets {
		if under(t.path, gone) {
			continue
		}
		if _, err := e.fs.Filesystem.Lstat(t.path); errors.Is(err, fs.ErrNotExist) {
			e.fs.nodes.remove(t.path)
			gone = append(gone, t.path)
		}
	}
}

// under reports whether p is one of roots or lies below one.
func under(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}
