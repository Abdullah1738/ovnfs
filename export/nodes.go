package export

import (
	"os"
	"path"
	"slices"
	"strings"
	"sync"
)

// Node ids come in two kinds. Counter ids, below derivedBit, are allocated
// in sequence and recorded in the identity log. Derived ids are derivedBit,
// the key of the parent directory and a 22-bit hash of the name: reading a
// fresh view hands them out without writing anything, and after a restart
// such an id is resolved again by listing the parent directory. A derived id
// is only handed out while its name is the one entry of that hash in the
// directory that no other node owns, the rule resolution applies.
//
// Every node with children is logged, under a counter id or under its
// derived id pinned in place, and owns a key: its id for counter nodes, a
// counter value for pinned ones. Records and derived ids name directories by
// key, so a directory keeps its children's ids wherever it moves; a node
// moved away from its derived name is pinned with its id instead.
//
// Every id stays below 1<<53: JavaScript runtimes report inode numbers as
// doubles, so larger ids would lose precision and distinct files could
// compare equal. A directory whose key does not fit 30 bits gives its new
// children logged counter ids. The split favours keys: running out of them
// would log every later view in full, while a hash collision costs one
// logged id.
const (
	derivedBit = 1 << 52
	hashBits   = 22
	// maxDerivingKey bounds the keys a derived id can carry.
	maxDerivingKey = 1 << (52 - hashBits)
	// idLimit bounds every id the table issues or accepts.
	idLimit = derivedBit << 1
	// ambiguous marks a name hash with several entries in a directory.
	ambiguous = "/"
)

// nodeTable gives every path the server has handed out a stable id, used as
// both its NFS file handle and its fileid. Ids live in a tree mirroring the
// namespace, so a rename re-parents one node in O(depth) and every handle
// below it keeps working, as with an inode-based server. Logged ids and
// namespace changes of logged nodes are appended to the identity log.
type nodeTable struct {
	mu        sync.Mutex
	root      *node
	byID      map[uint64]*node
	byKey     map[uint64]*node // logged nodes
	next      uint64           // next counter id or key
	list      func(dir string) ([]os.FileInfo, error)
	pending   []byte // records of the current operation, written by flush
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
	key    uint64 // 0 while the node is unlogged (derived, never recorded)
	name   string
	parent *node
	kids   map[string]*node
	dir    *dirState
}

// dirState is what the table knows about a directory's entries, enough to
// tell whether a child's derived id is unambiguous.
type dirState struct {
	// gen advances whenever entries may have appeared or the listing was
	// dropped; a listing read across a change is not installed.
	gen uint64
	// entries maps each name hash of the latest listing, plus names created
	// through the export since, to its name, or to ambiguous when several
	// entries share it. nil until listed.
	entries map[uint32]string
	// dups lists every name of each ambiguous hash of the latest listing.
	dups map[uint32][]string
	// retired holds the hashes of retired derived children: their ids are
	// never handed out again in this process.
	retired map[uint32]struct{}
}

// derivation is how ensure may identify a new child.
type derivation int

const (
	useCounter  derivation = iota // a logged counter id
	useDerived                    // the derived id
	needListing                   // unknown until the directory is listed
)

// newNodeTable creates the permanent export root identity. list reads a
// directory's entries for derived ids; nil hands out logged counter ids only.
func newNodeTable(list func(dir string) ([]os.FileInfo, error)) *nodeTable {
	root := &node{id: 1, key: 1}
	return &nodeTable{
		root:  root,
		byID:  map[uint64]*node{root.id: root},
		byKey: map[uint64]*node{root.key: root},
		next:  root.id + 1,
		list:  list,
	}
}

// id returns p's id, allocating ids for p and its ancestors on first use.
func (t *nodeTable) id(p string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer t.flush()
	var listed *node
	for try := 0; ; try++ {
		n, unlisted := t.ensure(p, listed, try < 3)
		if n != nil {
			return n.id
		}
		if ok, err := t.relist(unlisted); ok || err != nil {
			listed = unlisted
		}
	}
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
// unknown (never issued, removed, or replaced by a rename). A derived id
// issued before a restart is resolved through its parent directory.
func (t *nodeTable) path(id uint64) ([]string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.byID[id]
	if n == nil {
		if n = t.resolve(id); n == nil {
			return nil, false
		}
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
	defer t.flush()
	if from == to {
		return
	}
	src := t.find(from)
	if src == t.root {
		return
	}
	dir, name := split(to)
	d := t.dirNode(dir)
	if dst := d.kids[name]; dst != src {
		t.drop(dst)
	}
	if src != nil {
		delete(src.parent.kids, src.name)
		src.name, src.parent = name, d
		t.link(d, src)
		if src.key == 0 {
			// Its id no longer derives from its path.
			t.pin(src)
		} else {
			t.recordMove(src)
		}
	}
	t.appeared(d, name)
}

// remove retires the ids of p's subtree.
func (t *nodeTable) remove(p string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer t.flush()
	t.drop(t.find(p))
}

// created notes a directory entry made through the export at p, keeping the
// parent's listing complete. A name whose derived id another node holds, or
// one retired, is logged with a counter id right away, so a restart cannot
// resolve that id to it before it is ever handed out.
func (t *nodeTable) created(p string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer t.flush()
	dir, name := split(p)
	if d := t.find(dir); d != nil && d.kids[name] == nil {
		t.appeared(d, name)
	}
}

// listing identifies a directory read in progress: the node and the
// generation its listing must still have when the entries arrive.
type listing struct {
	dir *node
	gen uint64
}

// startListing is called before a directory read whose entries observe
// will record; reading a directory never handed out records nothing.
func (t *nodeTable) startListing(dir string) listing {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.find(dir)
	if n == nil {
		return listing{}
	}
	return listing{dir: n, gen: n.state().gen}
}

// observe installs the entries a directory read returned, letting the
// children it lists take derived ids without listing the directory again.
func (t *nodeTable) observe(l listing, infos []os.FileInfo) {
	if l.dir == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.install(l.dir, l.gen, infos)
}

// unlist drops the listings of p, and with deep of its whole subtree, after
// changes made behind the export.
func (t *nodeTable) unlist(p string, deep bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var walk func(*node)
	walk = func(n *node) {
		if s := n.dir; s != nil {
			s.gen++
			s.entries, s.dups = nil, nil
		}
		if deep {
			for _, k := range n.kids {
				walk(k)
			}
		}
	}
	if n := t.find(p); n != nil {
		walk(n)
	}
}

// ensure returns p's node, creating missing nodes, or the directory that
// must be listed before p's derived id can be decided. A directory just
// listed, or derive false, falls back to a counter id instead.
func (t *nodeTable) ensure(p string, listed *node, derive bool) (n, unlisted *node) {
	if p == "" {
		return t.root, nil
	}
	dir, name := split(p)
	d := t.dirNode(dir)
	if k := d.kids[name]; k != nil {
		return k, nil
	}
	if derive {
		switch id, how := t.derive(d, name); {
		case how == useDerived:
			k := &node{id: id, name: name, parent: d}
			t.link(d, k)
			return k, nil
		case how == needListing && d != listed:
			return nil, d
		}
	}
	return t.alloc(d, name), nil
}

// derive decides whether name, new in directory d, takes its derived id.
func (t *nodeTable) derive(d *node, name string) (uint64, derivation) {
	if t.list == nil || d.key >= maxDerivingKey {
		return 0, useCounter
	}
	h := nameHash(name)
	id := derivedID(d.key, h)
	if t.byID[id] != nil {
		return 0, useCounter
	}
	s := d.dir
	if s != nil {
		if _, gone := s.retired[h]; gone {
			return 0, useCounter
		}
	}
	if s == nil || s.entries == nil {
		return id, needListing
	}
	switch v, ok := s.entries[h]; {
	case v == name:
		return id, useDerived
	case !ok || v != ambiguous:
		return id, needListing // the listing predates name
	}
	names, ok := s.dups[h]
	if !ok || !slices.Contains(names, name) {
		return id, needListing
	}
	for _, other := range names {
		if other != name && d.kids[other] == nil {
			// Resolution could not tell the two apart.
			return 0, useCounter
		}
	}
	return id, useDerived
}

// resolve finds the node a derived id issued before a restart names: the one
// entry of its hash in the keyed directory that no other node owns.
func (t *nodeTable) resolve(id uint64) *node {
	if id&derivedBit == 0 || id >= idLimit || t.list == nil {
		return nil
	}
	key, h := id>>hashBits&(maxDerivingKey-1), uint32(id&(1<<hashBits-1))
	fresh := false
	for range 4 {
		d := t.byKey[key]
		if d == nil {
			return nil
		}
		if s := d.dir; s != nil {
			if _, gone := s.retired[h]; gone {
				return nil
			}
			if name, ok := s.candidate(d, h); ok {
				k := &node{id: id, name: name, parent: d}
				t.link(d, k)
				return k
			}
		}
		if fresh {
			return nil
		}
		installed, err := t.relist(d)
		if err != nil {
			return nil
		}
		if n := t.byID[id]; n != nil {
			return n
		}
		fresh = installed
	}
	return nil
}

// candidate returns the entry of hash h in d's listing that no node owns,
// if the listing knows exactly one.
func (s *dirState) candidate(d *node, h uint32) (string, bool) {
	v, ok := s.entries[h]
	switch {
	case !ok:
		return "", false
	case v != ambiguous:
		return v, d.kids[v] == nil
	}
	match, n := "", 0
	for _, name := range s.dups[h] {
		if d.kids[name] == nil {
			match, n = name, n+1
		}
	}
	return match, n == 1
}

// relist reads d again with the mutex released and installs the entries
// unless d changed meanwhile.
func (t *nodeTable) relist(d *node) (bool, error) {
	gen := d.state().gen
	dir := t.nodePath(d)
	t.mu.Unlock()
	infos, err := t.list(dir)
	t.mu.Lock()
	if err != nil {
		return false, err
	}
	return t.install(d, gen, infos), nil
}

// install replaces d's listing if d is live and unchanged since gen.
func (t *nodeTable) install(d *node, gen uint64, infos []os.FileInfo) bool {
	s := d.dir
	if t.byID[d.id] != d || s == nil || s.gen != gen {
		return false
	}
	s.entries, s.dups = make(map[uint32]string, len(infos)), nil
	for _, info := range infos {
		name := info.Name()
		h := nameHash(name)
		prev, ok := s.entries[h]
		if !ok {
			s.entries[h] = name
			continue
		}
		if s.dups == nil {
			s.dups = make(map[uint32][]string)
		}
		if prev != ambiguous {
			s.dups[h] = []string{prev}
			s.entries[h] = ambiguous
		}
		s.dups[h] = append(s.dups[h], name)
	}
	return true
}

// appeared records that name became an entry of d under the mutex.
func (t *nodeTable) appeared(d *node, name string) {
	h := nameHash(name)
	s := d.dir
	if s != nil {
		s.gen++
		if s.entries != nil {
			switch v, ok := s.entries[h]; {
			case !ok:
				s.entries[h] = name
			case v == name, v == ambiguous && slices.Contains(s.dups[h], name):
			default:
				s.entries[h] = ambiguous
				delete(s.dups, h)
			}
		}
	}
	if d.kids[name] != nil || t.list == nil || d.key == 0 || d.key >= maxDerivingKey {
		return
	}
	held := t.byID[derivedID(d.key, h)] != nil
	if s != nil {
		_, gone := s.retired[h]
		held = held || gone
	}
	if held {
		t.alloc(d, name)
	}
}

// dirNode returns the logged node of directory p, allocating missing
// directories and pinning unlogged ones, which are gaining a child.
func (t *nodeTable) dirNode(p string) *node {
	n := t.root
	for rest := p; rest != ""; {
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		k := n.kids[name]
		switch {
		case k == nil:
			k = t.alloc(n, name)
		case k.key == 0:
			t.pin(k)
		}
		n = k
	}
	return n
}

// alloc logs a new child of the logged directory d under a counter id.
func (t *nodeTable) alloc(d *node, name string) *node {
	k := &node{id: t.next, key: t.next, name: name, parent: d}
	t.next++
	t.link(d, k)
	t.byKey[k.key] = k
	t.recordNode(k)
	return k
}

// pin logs an unlogged node under its derived id and gives it a key.
func (t *nodeTable) pin(k *node) {
	k.key = t.next
	t.next++
	t.byKey[k.key] = k
	t.recordNode(k)
}

// link attaches k under d and indexes its id.
func (t *nodeTable) link(d, k *node) {
	if d.kids == nil {
		d.kids = make(map[string]*node)
	}
	d.kids[k.name] = k
	t.byID[k.id] = k
}

// state returns n's directory state, creating it on first use.
func (n *node) state() *dirState {
	if n.dir == nil {
		n.dir = &dirState{}
	}
	return n.dir
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

// drop retires an entire subtree under the mutex. Its derived ids stay
// reserved in their directories for the life of the process; the keys of
// dropped directories are never issued again, so ids derived from them die
// with them.
func (t *nodeTable) drop(n *node) {
	if n == nil || n == t.root {
		return
	}
	if n.key != 0 {
		t.recordRemove(n)
	}
	delete(n.parent.kids, n.name)
	var forget, retire func(*node)
	forget = func(n *node) {
		delete(t.byID, n.id)
		if n.key != 0 {
			delete(t.byKey, n.key)
		}
		for _, k := range n.kids {
			forget(k)
		}
	}
	retire = func(n *node) {
		if n.id&derivedBit != 0 {
			if d := t.byKey[n.id>>hashBits&(maxDerivingKey-1)]; d != nil {
				s := d.state()
				if s.retired == nil {
					s.retired = make(map[uint32]struct{})
				}
				s.retired[uint32(n.id&(1<<hashBits-1))] = struct{}{}
			}
		}
		for _, k := range n.kids {
			retire(k)
		}
	}
	forget(n)
	retire(n)
}

// derivedID is the id of the entry with name hash h in the directory keyed key.
func derivedID(key uint64, h uint32) uint64 {
	return derivedBit | key<<hashBits | uint64(h)
}

// nameHash is the 22-bit FNV-1a hash of name that derived ids carry; it must
// never change, or ids issued before an upgrade would resolve elsewhere.
func nameHash(name string) uint32 {
	h := uint64(14695981039346656037)
	for i := range len(name) {
		h ^= uint64(name[i])
		h *= 1099511628211
	}
	return uint32(h^h>>32) & (1<<hashBits - 1)
}

// split separates an export-relative path at its last slash.
func split(p string) (dir, name string) {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}
