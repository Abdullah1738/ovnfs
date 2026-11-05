//go:build linux

package watch

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

// mask selects the records telling of changes to a directory or its entries;
// IN_EXCL_UNLINK skips writes to unlinked files, which no path shows.
const mask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MODIFY | unix.IN_ATTRIB | unix.IN_CLOSE_WRITE |
	unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF |
	unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW | unix.IN_EXCL_UNLINK

// Watcher reports changes below added directories through one inotify
// instance, with a watch on every directory below a top (an outermost added
// directory); its methods are safe for concurrent use.
//
// A top is lost while it misses changes: after its directory was moved or
// deleted, or a directory below it could not be watched (as beyond the
// fs.inotify.max_user_watches limit).
type Watcher struct {
	forest[*node]
	fd int      // the inotify instance; used only under mu before closed
	f  *os.File // fd, read through the runtime poller so that Close interrupts reads

	// Guarded by forest.mu:
	wds   map[int32]*node  // every watched directory
	moved map[uint32]*node // directories renamed away during the current read, by cookie
	evs   []event          // changes found during the current read
	gen   uint64           // counts rescans
	err   error            // why reading stopped, once it did

	reader chan struct{} // closed when read returns
}

// node is a watched directory.
type node struct {
	wd     int32
	parent *node            // nil for a top or a directory renamed away
	name   string           // entry name in parent; the real path for a top
	kids   map[string]*node // watched subdirectories by entry name
	gen    uint64           // the last rescan that found it
	missed []string         // paths below it, relative, that changed while it was renamed away
}

// New starts a watcher that calls fn with batches of changed paths; fn runs on
// one goroutine at a time and must not block for long or call Close.
func New(fn func(paths []string)) (*Watcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, os.NewSyscallError("inotify_init1", err)
	}
	w := &Watcher{
		fd:     fd,
		f:      os.NewFile(uintptr(fd), "inotify"),
		wds:    map[int32]*node{},
		moved:  map[uint32]*node{},
		reader: make(chan struct{}),
	}
	w.init(w, fn)
	go w.read()
	return w, nil
}

// Add watches dir recursively; adding a watched dir again is a no-op. It fails
// if any directory below dir cannot be watched.
func (w *Watcher) Add(dir string) error { return w.add(dir) }

// Remove stops watching dir; once it returns fn gets no more paths for dir
// beyond a call already in flight.
func (w *Watcher) Remove(dir string) { w.remove(dir) }

// Covers reports whether every change below dir, an added directory, now
// reaches fn.
func (w *Watcher) Covers(dir string) bool { return w.covers(dir) }

// Close stops watching and returns once no fn call is in flight; fn is not
// called afterwards.
func (w *Watcher) Close() error {
	if !w.shut() {
		<-w.exited
		return nil
	}
	w.f.Close()
	<-w.reader
	w.stop()
	return nil
}

// canonical resolves the symlinks in path, which is absolute.
func canonical(path string) (string, error) { return filepath.EvalSymlinks(path) }

// watch implements backend.
func (w *Watcher) watch(real string) (*node, error) {
	if w.err != nil {
		return nil, w.err
	}
	n, err := w.visit(nil, real, real, false, false)
	if err != nil {
		if n != nil {
			w.drop(n)
		}
		return nil, err
	}
	return n, nil
}

// drop implements backend, keeping the trees of other tops that n absorbed.
func (w *Watcher) drop(n *node) {
	if n.parent != nil {
		return // absorbed by a top added above it
	}
	for k, o := range w.tops {
		if o != nil && o.parent != nil && o.root() == n {
			w.attach(o, nil, k)
		}
	}
	w.prune(n)
}

// read applies inotify records until Close.
func (w *Watcher) read() {
	defer close(w.reader)
	buf := make([]byte, 64<<10)
	for {
		n, err := w.f.Read(buf)
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			return
		}
		if err != nil {
			// Not seen in practice; nothing is covered from here on.
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			w.err = fmt.Errorf("watch: read inotify: %w", err)
			for k := range w.tops {
				w.lose(k)
			}
			w.mu.Unlock()
			return
		}
		w.handle(buf[:n])
		w.mu.Unlock()
	}
}

// handle applies one read's records; w.mu is held.
func (w *Watcher) handle(buf []byte) {
	for len(buf) >= unix.SizeofInotifyEvent {
		end := unix.SizeofInotifyEvent + int(binary.NativeEndian.Uint32(buf[12:]))
		if end > len(buf) {
			break
		}
		name, _, _ := bytes.Cut(buf[unix.SizeofInotifyEvent:end], []byte{0})
		w.apply(int32(binary.NativeEndian.Uint32(buf)), binary.NativeEndian.Uint32(buf[4:]), binary.NativeEndian.Uint32(buf[8:]), string(name))
		buf = buf[end:]
	}
	for c, n := range w.moved {
		delete(w.moved, c)
		if n.parent == nil && !w.isTop(n) {
			w.prune(n) // renamed out of every tree
		}
	}
	w.report(w.evs)
	w.evs = nil
}

// apply applies one record: watch wd's directory, or its entry name, changed.
func (w *Watcher) apply(wd int32, m, cookie uint32, name string) {
	if m&unix.IN_Q_OVERFLOW != 0 {
		w.overflow()
		return
	}
	n := w.wds[wd]
	if n == nil {
		return // unwatched since; its last records still arrive
	}
	switch {
	case m&unix.IN_IGNORED != 0:
		delete(w.wds, wd)
		w.vanish(n)
		return
	case m&(unix.IN_DELETE_SELF|unix.IN_MOVE_SELF) != 0:
		// The parent's watch reports the entry; a top's parent is unwatched.
		if w.isTop(n) {
			w.lose(n.name)
		}
		return
	}
	dir, away := w.locate(n)
	p := filepath.Join(dir, name)
	if away != nil {
		away.missed = append(away.missed, p)
		return
	}
	w.evs = append(w.evs, event{p, changed})
	if name == "" || m&unix.IN_ISDIR == 0 {
		return
	}
	switch {
	case m&unix.IN_MOVED_FROM != 0:
		if k := n.kids[name]; k != nil {
			delete(n.kids, name)
			k.parent = nil
			w.moved[cookie] = k
		}
	case m&unix.IN_MOVED_TO != 0:
		if k := w.moved[cookie]; k != nil {
			delete(w.moved, cookie)
			w.attach(k, n, name)
			for _, q := range k.missed {
				w.evs = append(w.evs, event{filepath.Join(p, q), changed})
			}
			k.missed = nil
			return
		}
		w.descend(n, name, p)
	case m&unix.IN_CREATE != 0:
		w.descend(n, name, p)
	case m&unix.IN_DELETE != 0:
		if k := n.kids[name]; k != nil {
			w.prune(k)
		}
	}
}

// vanish handles the kernel dropping n's watch: its directory was deleted, or
// its filesystem unmounted, showing what lies beneath the mount point.
func (w *Watcher) vanish(n *node) {
	if w.isTop(n) {
		w.lose(n.name)
		return
	}
	p := n.parent
	if p == nil || p.kids[n.name] != n {
		return
	}
	delete(p.kids, n.name)
	if dir, away := w.locate(p); away == nil {
		w.descend(p, n.name, filepath.Join(dir, n.name))
	}
}

// overflow rescans every top after the kernel dropped records, which may have
// announced directories still unwatched, and reports everything changed.
func (w *Watcher) overflow() {
	w.gen++
	for k, n := range w.tops {
		if n == nil {
			continue
		}
		m, err := w.visit(nil, k, k, false, true)
		if m != nil && m != n {
			// Another directory took the top's place unseen.
			w.prune(n)
			w.tops[k] = m
		}
		if err != nil {
			w.lose(k)
		}
	}
	w.evs = append(w.evs, event{kind: dropped})
}

// descend watches name, a directory new in parent found at path, reporting
// what it holds; failing that, it loses the top.
func (w *Watcher) descend(parent *node, name, path string) {
	if _, err := w.visit(parent, name, path, true, false); err != nil {
		w.lose(parent.root().name)
	}
}

// visit watches the directory at path, entry name of parent (nil: path is a
// top), and the directories below it not watched yet, or all of them when
// rescan is set. With report set, it records every entry below a newly watched
// directory as changed: entries may have appeared before the watch. It returns
// the directory's node, if watched, and any error but one showing that a
// directory below a top vanished or is unreadable.
func (w *Watcher) visit(parent *node, name, path string, report, rescan bool) (*node, error) {
	wd, n, err := w.addWatch(path)
	if err != nil {
		if parent != nil && skippable(err) {
			return nil, nil
		}
		return nil, &os.PathError{Op: "inotify_add_watch", Path: path, Err: err}
	}
	switch {
	case n == nil:
		n = &node{wd: wd}
		w.wds[wd] = n
	case n.above(parent):
		return nil, nil // a bind mount looping back into the tree
	case !rescan:
		w.attach(n, parent, name)
		return n, nil // watched along with everything below
	}
	n.gen = w.gen
	w.attach(n, parent, name)
	entries, err := os.ReadDir(path)
	if err != nil {
		if parent != nil && skippable(err) {
			return n, nil
		}
		return n, err
	}
	for _, e := range entries {
		p := filepath.Join(path, e.Name())
		if report {
			w.evs = append(w.evs, event{p, changed})
		}
		if e.IsDir() {
			if _, err := w.visit(n, e.Name(), p, report, rescan); err != nil {
				return n, err
			}
		}
	}
	if rescan {
		// Drop subdirectories whose removal was announced in lost records.
		for _, k := range n.kids {
			if k.gen != w.gen {
				w.prune(k)
			}
		}
	}
	return n, nil
}

// addWatch watches the directory at path, returning its node if it was
// watched already. A top found at another path moved there unseen: it is
// lost, as it no longer covers its own path.
func (w *Watcher) addWatch(path string) (int32, *node, error) {
	for {
		wd, err := unix.InotifyAddWatch(w.fd, path, mask)
		if err != nil {
			return 0, nil, err
		}
		n := w.wds[int32(wd)]
		if n == nil || !w.isTop(n) || n.name == path {
			return int32(wd), n, nil
		}
		w.lose(n.name)
	}
}

// skippable reports whether err shows a directory below a top vanished,
// turned into something else, or is unreadable: inotify needs read
// permission, without which the daemon cannot list it either.
func skippable(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EACCES)
}

// attach links n into parent as name, replacing what was there, or makes n
// the top at name when parent is nil.
func (w *Watcher) attach(n, parent *node, name string) {
	if n.parent != nil && n.parent.kids[n.name] == n {
		delete(n.parent.kids, n.name)
	}
	n.parent, n.name = parent, name
	if parent == nil {
		return
	}
	if old := parent.kids[name]; old != nil && old != n {
		w.prune(old)
	}
	if parent.kids == nil {
		parent.kids = map[string]*node{}
	}
	parent.kids[name] = n
}

// prune unlinks n and stops watching its tree.
func (w *Watcher) prune(n *node) {
	if n.parent != nil && n.parent.kids[n.name] == n {
		delete(n.parent.kids, n.name)
	}
	w.unwatch(n)
}

// unwatch removes the watches of n's tree.
func (w *Watcher) unwatch(n *node) {
	for _, k := range n.kids {
		w.unwatch(k)
	}
	if w.wds[n.wd] == n {
		delete(w.wds, n.wd)
		// Fails only for watches the kernel already dropped.
		_, _ = unix.InotifyRmWatch(w.fd, uint32(n.wd))
	}
}

// isTop reports whether n is the root of a top's tree.
func (w *Watcher) isTop(n *node) bool { return n.parent == nil && w.tops[n.name] == n }

// root returns the root of n's tree: a top, or a directory renamed away.
func (n *node) root() *node {
	for n.parent != nil {
		n = n.parent
	}
	return n
}

// above reports whether n is p or one of its ancestors.
func (n *node) above(p *node) bool {
	for ; p != nil; p = p.parent {
		if p == n {
			return true
		}
	}
	return false
}

// locate returns n's path, or, below a directory renamed away that has not
// arrived yet, n's path relative to it and that directory.
func (w *Watcher) locate(n *node) (string, *node) {
	var names []string
	for ; n.parent != nil; n = n.parent {
		names = append(names, n.name)
	}
	top := w.isTop(n)
	if top {
		names = append(names, n.name)
	}
	slices.Reverse(names)
	if top {
		return filepath.Join(names...), nil
	}
	return filepath.Join(names...), n
}
