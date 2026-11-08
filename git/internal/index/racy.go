package index

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// smudgeRacy truncates the cached size of racily clean entries to zero
// (technical/racy-git), so that a later stat comparison cannot mistake a
// file modified in the same second as the index for unchanged.
//
// An entry is racy when its mtime is in or after the second of ref; like git
// built without USE_NSEC (the default), only whole seconds are compared, both
// here and in the stat comparison below.
func (idx *Index) smudgeRacy(ref time.Time, opts WriteOptions) {
	if ref.IsZero() {
		return
	}
	refSec := uint32(ref.Unix())
	for i := range idx.Entries {
		e := &idx.Entries[i]
		if e.Stat.Size == 0 || e.Stat.MTime.Sec < refSec || e.Mode == object.ModeGitlink || e.Mode == object.ModeTree {
			continue
		}
		if opts.Worktree == "" || modifiedInPlace(opts, e) {
			e.Stat.Size = 0
		}
	}
}

// modifiedInPlace reports whether e's file still matches its cached stat
// data while its content no longer hashes to e.ID: the case where only
// smudging keeps the change visible. A missing file, or one whose stat data
// already differs, needs no smudge.
func modifiedInPlace(opts WriteOptions, e *Entry) bool {
	name := filepath.Join(opts.Worktree, filepath.FromSlash(e.Path))
	info, err := os.Lstat(name)
	if err != nil {
		return false
	}
	symlink := e.Mode == object.ModeSymlink
	switch {
	case symlink && info.Mode()&fs.ModeSymlink == 0, !symlink && !info.Mode().IsRegular():
		return false
	case uint32(info.Size()) != e.Stat.Size, uint32(info.ModTime().Unix()) != e.Stat.MTime.Sec:
		return false
	}
	if st, ok := platformStat(name); ok {
		if !opts.IgnoreCTime && st.CTime.Sec != e.Stat.CTime.Sec {
			return false
		}
		if !opts.MinimalStat && (st.Ino != e.Stat.Ino || st.UID != e.Stat.UID || st.GID != e.Stat.GID) {
			return false
		}
	}
	var content []byte
	if symlink {
		target, err := os.Readlink(name)
		if err != nil {
			return false
		}
		content = []byte(target)
	} else if content, err = os.ReadFile(name); err != nil {
		return false
	}
	return object.Hash(e.ID.Format(), object.Blob, content) != e.ID
}
