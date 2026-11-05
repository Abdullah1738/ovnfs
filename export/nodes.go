package export

import (
	"os"
	"path"
	"strings"
	"sync"
)

// nodeTable gives every path the server has handed out a stable id, used as
// both its NFS file handle and its fileid. Ids live in a tree mirroring the
// namespace, so a rename re-parents one node in O(depth) and every handle
// below it keeps working, as with an inode-based server. Allocations and
// namespace changes are appended to the persistent identity log.
type nodeTable struct {
	mu        sync.Mutex
	root      *node
	byID      map[uint64]*node
	next      uint64
	log       *os.File
	logErr    error
	dirty     bool
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// node links an identity to its current parent and basename.
type node struct {
	id     uint64
	name   string
	parent *node
	kids   map[string]*node
}

// newNodeTable creates the permanent export root identity.
func newNodeTable() *nodeTable {
	root := &node{id: 1}
	return &nodeTable{root: root, byID: map[uint64]*node{root.id: root}, next: root.id + 1}
}

// id returns p's id, allocating ids for p and its ancestors on first use.
func (t *nodeTable) id(p string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ensure(p).id
}

// lookup returns p's id without allocating: a path the server never handed
// out has no handle a client could cache.
func (t *nodeTable) lookup(p string) (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := t.find(p); n != nil {
		return n.id, true
	}
	return 0, false
}

// nodeRef is one issued id with its current path.
type nodeRef struct {
	id   uint64
	path string
}

// subtree lists p and every issued descendant, parents before children.
func (t *nodeTable) subtree(p string) []nodeRef {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.find(p)
	if n == nil {
		return nil
	}
	var out []nodeRef
	var walk func(*node, string)
	walk = func(n *node, p string) {
		out = append(out, nodeRef{id: n.id, path: p})
		for name, k := range n.kids {
			walk(k, path.Join(p, name))
		}
	}
	walk(n, p)
	return out
}

// path returns the current path components of id, or false if the id is
// unknown (never issued, removed, or replaced by a rename).
func (t *nodeTable) path(id uint64) ([]string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.byID[id]
	if !ok {
		return nil, false
	}
	depth := 0
	for c := n; c.parent != nil; c = c.parent {
		depth++
	}
	out := make([]string, depth)
	for c := n; c.parent != nil; c = c.parent {
		depth--
		out[depth] = c.name
	}
	return out, true
}

// rename moves from's subtree to to, retiring whatever ids to held.
func (t *nodeTable) rename(from, to string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if from == to {
		return
	}
	dir, name := split(to)
	p := t.ensure(dir)
	t.record(nodeRecord{Op: "rename", Path: from, To: to})
	n := t.find(from)
	t.drop(t.find(to))
	if n == nil || n == t.root {
		return
	}
	delete(n.parent.kids, n.name)
	n.name, n.parent = name, p
	if p.kids == nil {
		p.kids = make(map[string]*node)
	}
	p.kids[name] = n
}

// remove retires the ids of p's subtree.
func (t *nodeTable) remove(p string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.record(nodeRecord{Op: "remove", Path: p})
	t.drop(t.find(p))
}

// ensure allocates missing ancestors in parent-first order under the mutex.
func (t *nodeTable) ensure(p string) *node {
	n := t.root
	for rest := p; rest != ""; {
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		k := n.kids[name]
		if k == nil {
			k = &node{id: t.next, name: name, parent: n}
			t.next++
			if n.kids == nil {
				n.kids = make(map[string]*node)
			}
			n.kids[name] = k
			t.byID[k.id] = k
			t.record(nodeRecord{Op: "alloc", ID: k.id, Path: t.nodePath(k)})
		}
		n = k
	}
	return n
}

// nodePath returns an export-relative path while the mutex is held.
func (t *nodeTable) nodePath(n *node) string {
	if n.parent == nil {
		return ""
	}
	if n.parent == t.root {
		return n.name
	}
	return t.nodePath(n.parent) + "/" + n.name
}

// find resolves a path without allocating under the mutex.
func (t *nodeTable) find(p string) *node {
	n := t.root
	for rest := p; rest != "" && n != nil; {
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		n = n.kids[name]
	}
	return n
}

// drop retires an entire subtree under the mutex.
func (t *nodeTable) drop(n *node) {
	if n == nil || n == t.root {
		return
	}
	delete(n.parent.kids, n.name)
	var forget func(*node)
	forget = func(n *node) {
		delete(t.byID, n.id)
		for _, k := range n.kids {
			forget(k)
		}
	}
	forget(n)
}

// split separates an export-relative path at its last slash.
func split(p string) (dir, name string) {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}
