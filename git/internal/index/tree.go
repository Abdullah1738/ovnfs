package index

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// FromTree returns the index `git read-tree <tree>` writes into a fresh index
// file: a stage-0 entry with zero stat data for every file of the tree root,
// and a fully valid cache tree.
func FromTree(read TreeReader, root object.ID, f object.Format) (*Index, error) {
	w := treeWalk{read: read}
	node, err := w.tree(root, "", "", 0)
	if err != nil {
		return nil, err
	}
	if err := checkOrder(w.out); err != nil {
		return nil, fmt.Errorf("tree %s: %w", root, err)
	}
	idx := New(f)
	idx.Entries = w.out
	idx.CacheTree = node
	return idx, nil
}

// ExpandSparse replaces every sparse-directory entry by the files of its
// tree, marked skip-worktree with zero stat data, the full index git works on
// whenever a command is not sparse-aware. The cache tree gains the expanded
// directories' nodes.
func (idx *Index) ExpandSparse(read TreeReader) error {
	if !idx.Sparse() {
		return nil
	}
	w := treeWalk{read: read, skipWorktree: true, out: make([]Entry, 0, len(idx.Entries))}
	for i := range idx.Entries {
		e := &idx.Entries[i]
		if !e.IsSparseDir() {
			w.out = append(w.out, *e)
			continue
		}
		dir := strings.TrimSuffix(e.Path, "/")
		name := dir[strings.LastIndexByte(dir, '/')+1:]
		node, err := w.tree(e.ID, name, e.Path, 0)
		if err != nil {
			return err
		}
		if idx.CacheTree != nil {
			idx.CacheTree.graft(dir, node)
		}
	}
	if err := checkOrder(w.out); err != nil {
		return fmt.Errorf("expanding sparse index: %w", err)
	}
	idx.Entries = w.out
	return nil
}

// graft replaces the node of directory dir, which stood for one sparse
// entry, by node, and grows the valid ancestors' entry counts to match.
func (t *CacheTree) graft(dir string, node *CacheTree) {
	grow := node.Entries - 1
	parent := t
	for {
		if parent.Valid() {
			parent.Entries += grow
		}
		first, rest, ok := strings.Cut(dir, "/")
		if !ok {
			parent.setChild(node)
			return
		}
		if parent = parent.child(first); parent == nil {
			return
		}
		dir = rest
	}
}

// treeWalk appends the files of trees to out in index order.
type treeWalk struct {
	read         TreeReader
	skipWorktree bool
	out          []Entry
}

// maxTreeDepth bounds recursion through (possibly cyclic, corrupt) trees.
const maxTreeDepth = 4096

// tree appends the files of tree id, whose directory path is prefix (empty
// or ending in '/'), and returns its cache-tree node named name.
func (w *treeWalk) tree(id object.ID, name, prefix string, depth int) (*CacheTree, error) {
	if depth > maxTreeDepth {
		return nil, fmt.Errorf("tree %s: nested too deeply", id)
	}
	entries, err := w.read(id)
	if err != nil {
		return nil, fmt.Errorf("read tree %s: %w", id, err)
	}
	node := &CacheTree{Name: name, ID: id}
	start := len(w.out)
	for _, te := range entries {
		if te.Mode == object.ModeTree {
			child, err := w.tree(te.ID, te.Name, prefix+te.Name+"/", depth+1)
			if err != nil {
				return nil, err
			}
			node.Children = append(node.Children, child)
			continue
		}
		w.out = append(w.out, Entry{Path: prefix + te.Name, ID: te.ID, Mode: te.Mode, SkipWorktree: w.skipWorktree})
	}
	node.Entries = len(w.out) - start
	slices.SortFunc(node.Children, func(a, b *CacheTree) int { return compareChildName(a.Name, b.Name) })
	return node, nil
}
