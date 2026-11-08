package index

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// CacheTree is a node of the TREE extension: a directory whose span of index
// entries is known to hash to an existing tree, unless invalidated.
type CacheTree struct {
	Name string // path component relative to the parent; "" for the root
	// Entries is the number of index entries under this directory, -1 when the
	// node is invalid (an entry below it changed since ID was computed).
	Entries int
	ID      object.ID // zero when invalid
	// Children are the subdirectory nodes in git's order: shorter names
	// first, equal lengths bytewise.
	Children []*CacheTree
}

// Valid reports whether ID names the tree of the node's entries.
func (t *CacheTree) Valid() bool { return t.Entries >= 0 }

func (t *CacheTree) invalidate() {
	t.Entries = -1
	t.ID = object.ID{}
}

func compareChildName(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
}

// child returns the subdirectory node named name, or nil.
func (t *CacheTree) child(name string) *CacheTree {
	i, ok := slices.BinarySearchFunc(t.Children, name, func(c *CacheTree, name string) int {
		return compareChildName(c.Name, name)
	})
	if !ok {
		return nil
	}
	return t.Children[i]
}

// setChild inserts or replaces the subdirectory node c.
func (t *CacheTree) setChild(c *CacheTree) {
	i, ok := slices.BinarySearchFunc(t.Children, c.Name, func(c *CacheTree, name string) int {
		return compareChildName(c.Name, name)
	})
	if ok {
		t.Children[i] = c
		return
	}
	t.Children = slices.Insert(t.Children, i, c)
}

// invalidatePath marks the nodes of path's parent directories invalid and
// drops the node of path itself, as git does whenever an entry at path is
// added, replaced or removed (even when nothing was there to remove).
func (t *CacheTree) invalidatePath(path string) {
	node := t
	for {
		node.invalidate()
		dir, rest, ok := strings.Cut(path, "/")
		if !ok {
			node.Children = slices.DeleteFunc(node.Children, func(c *CacheTree) bool { return c.Name == path })
			return
		}
		if node = node.child(dir); node == nil {
			return
		}
		path = rest
	}
}

var errCacheTree = errors.New("malformed cache tree")

func parseCacheTree(data []byte, f object.Format) (*CacheTree, error) {
	root, rest, err := parseCacheTreeNode(data, f, 0)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", errCacheTree, len(rest))
	}
	return root, nil
}

// maxCacheTreeDepth bounds recursion on corrupt input; real paths nest far less.
const maxCacheTreeDepth = 4096

func parseCacheTreeNode(b []byte, f object.Format, depth int) (*CacheTree, []byte, error) {
	if depth > maxCacheTreeDepth {
		return nil, nil, fmt.Errorf("%w: nested too deeply", errCacheTree)
	}
	nul := bytes.IndexByte(b, 0)
	if nul < 0 {
		return nil, nil, errCacheTree
	}
	t := &CacheTree{Name: string(b[:nul])}
	b = b[nul+1:]
	sp := bytes.IndexByte(b, ' ')
	nl := bytes.IndexByte(b, '\n')
	if sp < 0 || nl < sp {
		return nil, nil, errCacheTree
	}
	entries, err1 := strconv.Atoi(string(b[:sp]))
	subtrees, err2 := strconv.Atoi(string(b[sp+1 : nl]))
	if err1 != nil || err2 != nil || subtrees < 0 {
		return nil, nil, errCacheTree
	}
	b = b[nl+1:]
	if entries >= 0 {
		if len(b) < f.Size() {
			return nil, nil, errCacheTree
		}
		t.Entries = entries
		t.ID, _ = object.IDFromRaw(f, b[:f.Size()])
		b = b[f.Size():]
	} else {
		t.Entries = -1
	}
	// Each child needs at least four bytes ("\x000 0\n" minus its name).
	if subtrees > len(b)/4 {
		return nil, nil, errCacheTree
	}
	t.Children = make([]*CacheTree, 0, subtrees)
	for range subtrees {
		var c *CacheTree
		if c, b, err1 = parseCacheTreeNode(b, f, depth+1); err1 != nil {
			return nil, nil, err1
		}
		t.Children = append(t.Children, c)
	}
	return t, b, nil
}

// appendCacheTree serializes t top-down, depth-first, as the TREE extension.
func appendCacheTree(b []byte, t *CacheTree) []byte {
	b = append(b, t.Name...)
	b = append(b, 0)
	entries := max(t.Entries, -1)
	b = strconv.AppendInt(b, int64(entries), 10)
	b = append(b, ' ')
	b = strconv.AppendInt(b, int64(len(t.Children)), 10)
	b = append(b, '\n')
	if entries >= 0 {
		b = append(b, t.ID.Raw()...)
	}
	for _, c := range t.Children {
		b = appendCacheTree(b, c)
	}
	return b
}
