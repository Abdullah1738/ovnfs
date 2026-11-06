package export

import (
	"errors"
	"os"
	"path"
	"sync/atomic"
	"syscall"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/overlay"
)

// nodeFS gives both protocols stable node ids and keeps their paths in step
// with renames and removals, independently of overlay copy-up.
type nodeFS struct {
	billy.Filesystem
	billy.Change
	nodes    *nodeTable
	onRecall atomic.Pointer[func(id uint64)] // see Export.OnRecall
}

// ReadDir hides [overlay.AppleDouble] sidecars (fs::copy and cp copy their
// xattrs too). They must stay creatable and reachable by name, or xattr
// writes and therefore copies fail, but listing them would show tools like
// git and cargo a file per real file.
//
// The complete listing also tells the node table which children may take
// derived ids.
func (n *nodeFS) ReadDir(name string) ([]os.FileInfo, error) {
	l := n.nodes.startListing(overlay.Clean(name))
	infos, err := n.Filesystem.ReadDir(name)
	if err != nil {
		return nil, err
	}
	n.nodes.observe(l, infos)
	out := infos[:0]
	for _, info := range infos {
		if !overlay.AppleDouble(info.Name()) {
			out = append(out, info)
		}
	}
	return out, nil
}

// Link forwards hard links without coupling the protocol to the router type.
func (n *nodeFS) Link(oldname, newname string) error {
	n.recall(oldname) // its link count changes
	fs, ok := n.Filesystem.(interface{ Link(string, string) error })
	if !ok {
		return syscall.ENOTSUP
	}
	if err := fs.Link(oldname, newname); err != nil {
		return err
	}
	n.nodes.created(overlay.Clean(newname))
	return nil
}

// MkdirAll creates a directory and notes the new entry for node ids.
func (n *nodeFS) MkdirAll(name string, perm os.FileMode) error {
	if err := n.Filesystem.MkdirAll(name, perm); err != nil {
		return err
	}
	n.nodes.created(overlay.Clean(name))
	return nil
}

// Symlink creates a symbolic link and notes the new entry for node ids.
func (n *nodeFS) Symlink(target, link string) error {
	if err := n.Filesystem.Symlink(target, link); err != nil {
		return err
	}
	n.nodes.created(overlay.Clean(link))
	return nil
}

// TempFile creates a uniquely named file and notes the new entry for node ids.
func (n *nodeFS) TempFile(dir, prefix string) (billy.File, error) {
	f, err := n.Filesystem.TempFile(dir, prefix)
	if err != nil {
		return nil, err
	}
	n.nodes.created(overlay.Clean(f.Name()))
	return f, nil
}

// Rename moves a node without changing its id or those of its descendants.
func (n *nodeFS) Rename(from, to string) error {
	n.recall(from, to)
	if err := n.Filesystem.Rename(from, to); err != nil {
		return err
	}
	src, dst := overlay.Clean(from), overlay.Clean(to)
	if src == dst {
		return nil
	}
	n.nodes.rename(src, dst)
	return nil
}

// Remove retires a deleted node and tolerates hidden sidecars in empty dirs.
func (n *nodeFS) Remove(name string) error {
	n.recall(name)
	err := n.Filesystem.Remove(name)
	if errors.Is(err, syscall.ENOTEMPTY) && n.dropAppleDouble(name) {
		err = n.Filesystem.Remove(name)
	}
	if err != nil {
		return err
	}
	n.nodes.remove(overlay.Clean(name))
	return nil
}

// dropAppleDouble removes the "._*" files from dir if nothing else is in it.
// Clients cannot see them (ReadDir hides them), so to the client the
// directory is empty and rmdir must succeed.
func (n *nodeFS) dropAppleDouble(dir string) bool {
	infos, err := n.Filesystem.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, info := range infos {
		if !overlay.AppleDouble(info.Name()) {
			return false
		}
	}
	for _, info := range infos {
		if n.Filesystem.Remove(path.Join(dir, info.Name())) != nil {
			return false
		}
		n.nodes.remove(overlay.Clean(path.Join(dir, info.Name())))
	}
	return true
}
