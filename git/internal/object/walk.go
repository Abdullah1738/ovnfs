package object

import (
	"errors"
	"fmt"
	"iter"
)

// readTree reads and decodes tree id; the zero ID is the empty tree.
func (db *DB) readTree(id ID) ([]TreeEntry, error) {
	if id.IsZero() {
		return nil, nil
	}
	kind, data, err := db.Read(id)
	if err != nil {
		return nil, err
	}
	if kind != Tree {
		return nil, fmt.Errorf("object %s is a %s, not a tree", id, kind)
	}
	entries, err := ParseTree(db.format, data)
	if err != nil {
		return nil, fmt.Errorf("tree %s: %w", id, err)
	}
	return entries, nil
}

// WalkOptions configures Walk.
type WalkOptions struct {
	// Trees also yields each subtree, before its contents, as
	// git ls-tree -r -t does.
	Trees bool
}

// Walk yields the entries below root (a tree, or a commit or tag peeling to
// one) recursively, in git ls-tree -r --full-tree order, with Name set to the
// entry's full slash-separated path. Gitlinks are leaves. A failure is
// yielded once, with a zero entry, and ends the walk.
func (db *DB) Walk(root ID, opts WalkOptions) iter.Seq2[TreeEntry, error] {
	return func(yield func(TreeEntry, error) bool) {
		tree, err := db.Peel(root, Tree)
		if err != nil {
			yield(TreeEntry{}, err)
			return
		}
		if err := db.walk(tree, "", opts, yield); err != nil && err != errStopWalk {
			yield(TreeEntry{}, err)
		}
	}
}

// errStopWalk unwinds the recursion when the consumer stops iterating.
var errStopWalk = errors.New("walk stopped")

func (db *DB) walk(tree ID, prefix string, opts WalkOptions, yield func(TreeEntry, error) bool) error {
	entries, err := db.readTree(tree)
	if err != nil {
		return err
	}
	for _, e := range entries {
		e.Name = prefix + e.Name
		if e.Mode != ModeTree {
			if !yield(e, nil) {
				return errStopWalk
			}
			continue
		}
		if opts.Trees && !yield(e, nil) {
			return errStopWalk
		}
		if err := db.walk(e.ID, e.Name+"/", opts, yield); err != nil {
			return err
		}
	}
	return nil
}

// Change is one difference between two trees, as a line of
// git diff-tree -r --raw --no-renames.
type Change struct {
	Path string
	// Old and New are the entry on each side, Name holding the full path;
	// the side where the path is absent is the zero TreeEntry.
	Old, New TreeEntry
	// Status is 'A' (added), 'D' (deleted), 'M' (content or mode changed)
	// or 'T' (type changed between file, symlink and gitlink).
	Status byte
}

// DiffTrees returns the leaf changes from tree a to tree b (either may be a
// commit or tag peeling to a tree, or the zero ID for the empty tree), equal
// to git diff-tree -r --raw --no-renames a b: in the same order, trees never
// reported, and a file replaced by a directory (or the reverse) shown as the
// deletion and additions of the leaves involved.
func (db *DB) DiffTrees(a, b ID) ([]Change, error) {
	var err error
	if !a.IsZero() {
		if a, err = db.Peel(a, Tree); err != nil {
			return nil, err
		}
	}
	if !b.IsZero() {
		if b, err = db.Peel(b, Tree); err != nil {
			return nil, err
		}
	}
	var changes []Change
	err = db.diff(a, b, "", &changes)
	return changes, err
}

func (db *DB) diff(a, b ID, prefix string, changes *[]Change) error {
	if a == b || (a.IsZero() && b.IsZero()) {
		return nil
	}
	old, err := db.readTree(a)
	if err != nil {
		return err
	}
	cur, err := db.readTree(b)
	if err != nil {
		return err
	}
	zero := db.format.Zero()
	for len(old) > 0 || len(cur) > 0 {
		c := 0
		switch {
		case len(old) == 0:
			c = 1
		case len(cur) == 0:
			c = -1
		default:
			c = CompareEntries(old[0], cur[0])
		}
		switch {
		case c < 0:
			e := old[0]
			old = old[1:]
			if e.Mode == ModeTree {
				err = db.diff(e.ID, zero, prefix+e.Name+"/", changes)
			} else {
				e.Name = prefix + e.Name
				*changes = append(*changes, Change{Path: e.Name, Old: e, Status: 'D'})
			}
		case c > 0:
			e := cur[0]
			cur = cur[1:]
			if e.Mode == ModeTree {
				err = db.diff(zero, e.ID, prefix+e.Name+"/", changes)
			} else {
				e.Name = prefix + e.Name
				*changes = append(*changes, Change{Path: e.Name, New: e, Status: 'A'})
			}
		default:
			o, n := old[0], cur[0]
			old, cur = old[1:], cur[1:]
			switch {
			case o.Mode == ModeTree:
				err = db.diff(o.ID, n.ID, prefix+o.Name+"/", changes)
			case o.Mode != n.Mode || o.ID != n.ID:
				status := byte('M')
				if o.Mode&0o170000 != n.Mode&0o170000 {
					status = 'T'
				}
				o.Name = prefix + o.Name
				n.Name = o.Name
				*changes = append(*changes, Change{Path: o.Name, Old: o, New: n, Status: status})
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}
