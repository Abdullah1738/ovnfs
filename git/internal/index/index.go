// Package index reads, edits and writes git's index file ("dircache"):
// versions 2, 3 and 4, split and sparse indexes, and the extensions git
// records alongside the entries.
//
// It is written from git's documentation (gitformat-index, racy-git,
// sparse-index, git-update-index) and from observing the git binary, not
// from another implementation. Tree objects come in through a TreeReader, so
// the package does not depend on an object database.
package index

import (
	"cmp"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// TreeReader returns the entries of the tree object id. Callers back it with
// their object database.
type TreeReader func(id object.ID) ([]object.TreeEntry, error)

// ErrLocked reports that "<index>.lock" already exists: another process is
// writing the index.
var ErrLocked = errors.New("index file is locked")

// ErrSparseDirectory reports an edit to a path inside a sparse-directory
// entry; ExpandSparse the index first.
var ErrSparseDirectory = errors.New("path is inside a sparse directory entry")

// Time is a timestamp as the index stores it: seconds and nanoseconds, each
// truncated to 32 bits.
type Time struct {
	Sec, Nsec uint32
}

// Stat is the stat(2) data cached for an entry so that unchanged files need
// not be re-hashed. Every field is truncated to 32 bits.
type Stat struct {
	CTime, MTime             Time
	Dev, Ino, UID, GID, Size uint32
}

// Entry is one index entry: a staged path at a merge stage.
type Entry struct {
	Path  string // slash-separated; a sparse directory ends in '/'
	ID    object.ID
	Mode  object.Mode
	Stage uint8 // 0 normally; 1 (base), 2 (ours), 3 (theirs) during a merge
	Stat  Stat

	AssumeValid  bool // "assume unchanged" (update-index --assume-unchanged)
	SkipWorktree bool // outside the sparse checkout
	IntentToAdd  bool // "git add -N"
}

// IsSparseDir reports a sparse-directory entry: a whole tree outside the
// sparse-checkout cone, recorded as one entry.
func (e *Entry) IsSparseDir() bool {
	return e.Mode == object.ModeTree && e.SkipWorktree && strings.HasSuffix(e.Path, "/")
}

func (e *Entry) extended() bool { return e.SkipWorktree || e.IntentToAdd }

// compareEntry is the index order: bytewise path, then stage.
func compareEntry(a, b *Entry) int {
	return cmp.Or(strings.Compare(a.Path, b.Path), cmp.Compare(a.Stage, b.Stage))
}

// Index is a decoded index file.
type Index struct {
	Format object.Format
	// Version is the on-disk version read (2, 3 or 4). Write keeps 4 and
	// otherwise chooses 2 or 3 by whether extended flags are in use, as git does.
	Version uint32
	// Entries is sorted in index order (bytewise path, then stage).
	Entries []Entry

	CacheTree   *CacheTree    // TREE extension; nil when absent
	ResolveUndo []ResolveUndo // REUC extension
	Untracked   *UntrackedCache
	FSMonitor   *FSMonitor

	// SkipHash records an all-zero trailing checksum (index.skipHash); Write
	// keeps writing that way.
	SkipHash bool
	// ModTime is the mtime of the file read. Entries modified in or after its
	// second are racily clean; Write smudges them (see racy-git).
	ModTime time.Time
}

// New returns an empty index for a repository of format f.
func New(f object.Format) *Index {
	return &Index{Format: f, Version: 2}
}

// Find returns the position of path at stage in Entries, or where it would be
// inserted, and whether it is present.
func (idx *Index) Find(path string, stage uint8) (int, bool) {
	lo, hi := 0, len(idx.Entries)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		e := &idx.Entries[mid]
		switch c := cmp.Or(strings.Compare(e.Path, path), cmp.Compare(e.Stage, stage)); {
		case c == 0:
			return mid, true
		case c < 0:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return lo, false
}

// Sparse reports whether any sparse-directory entry remains.
func (idx *Index) Sparse() bool {
	return slices.ContainsFunc(idx.Entries, func(e Entry) bool { return e.IsSparseDir() })
}
