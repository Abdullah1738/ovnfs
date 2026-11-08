package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// WriteOptions tunes Write and WriteFile.
type WriteOptions struct {
	// RacyTime is the mtime of the index file being replaced, the reference
	// for racy-git smudging; zero means idx.ModTime.
	RacyTime time.Time
	// Worktree is the work tree root. When set, a racily clean entry is
	// smudged only if its file still matches the cached stat data but no
	// longer the staged object, as git does; when empty every racily clean
	// entry is smudged, which costs git a re-hash but is never wrong.
	Worktree string
	// IgnoreCTime mirrors core.trustCTime=false for the stat comparison.
	IgnoreCTime bool
	// MinimalStat mirrors core.checkStat=minimal: inode, uid and gid are not
	// compared.
	MinimalStat bool
}

// Write writes idx to the index file name with git's lock protocol: the
// content goes to "<name>.lock", created exclusively, which is then renamed
// over name. An existing lock fails with an error satisfying
// errors.Is(err, ErrLocked). Racily clean entries are smudged first (see
// WriteOptions), and idx.ModTime becomes the new file's mtime.
func Write(name string, idx *Index, opts WriteOptions) error {
	lock := name + ".lock"
	file, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w: %s exists", ErrLocked, lock)
	}
	if err != nil {
		return err
	}
	_, err = file.Write(idx.encode(opts))
	// Close before renaming: Windows cannot rename an open file.
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(lock, name)
	}
	if err != nil {
		os.Remove(lock)
		return fmt.Errorf("write %s: %w", name, err)
	}
	return idx.recordModTime(name)
}

// WriteFile writes idx to exactly path, creating or truncating it, for
// callers that manage their own temporary files. Racily clean entries are
// smudged first, and idx.ModTime becomes the file's mtime.
func WriteFile(path string, idx *Index, opts WriteOptions) error {
	if err := os.WriteFile(path, idx.encode(opts), 0o666); err != nil {
		return err
	}
	return idx.recordModTime(path)
}

func (idx *Index) recordModTime(name string) error {
	info, err := os.Stat(name)
	if err != nil {
		return err
	}
	idx.ModTime = info.ModTime()
	return nil
}

// writeVersion is the version git writes idx in: 4 stays 4; otherwise 3 if
// any entry needs extended flags, else 2.
func (idx *Index) writeVersion() uint32 {
	if idx.Version == 4 {
		return 4
	}
	for i := range idx.Entries {
		if idx.Entries[i].extended() {
			return 3
		}
	}
	return 2
}

// encode smudges racily clean entries and serializes idx: entries, then the
// TREE, REUC and sdir extensions in the order git writes them. Split-index,
// untracked-cache, fsmonitor and offset-table data are not written.
func (idx *Index) encode(opts WriteOptions) []byte {
	ref := opts.RacyTime
	if ref.IsZero() {
		ref = idx.ModTime
	}
	idx.smudgeRacy(ref, opts)

	version := idx.writeVersion()
	hs := idx.Format.Size()
	b := make([]byte, 0, 12+len(idx.Entries)*(statSize+hs+40)+hs)
	b = append(b, signature...)
	b = binary.BigEndian.AppendUint32(b, version)
	b = binary.BigEndian.AppendUint32(b, uint32(len(idx.Entries)))
	prev := ""
	for i := range idx.Entries {
		b = appendEntry(b, &idx.Entries[i], version, prev)
		prev = idx.Entries[i].Path
	}
	if idx.CacheTree != nil {
		start := beginExtension(&b, "TREE")
		b = appendCacheTree(b, idx.CacheTree)
		endExtension(b, start)
	}
	if len(idx.ResolveUndo) > 0 {
		start := beginExtension(&b, "REUC")
		b = appendResolveUndo(b, idx.ResolveUndo)
		endExtension(b, start)
	}
	if idx.Sparse() {
		endExtension(b, beginExtension(&b, "sdir"))
	}
	if idx.SkipHash {
		return append(b, make([]byte, hs)...)
	}
	h := idx.Format.New()
	h.Write(b)
	return h.Sum(b)
}

// beginExtension appends an extension header with a size to be filled in by
// endExtension, and returns where the payload starts.
func beginExtension(b *[]byte, sig string) int {
	*b = append(*b, sig...)
	*b = append(*b, 0, 0, 0, 0)
	return len(*b)
}

func endExtension(b []byte, start int) {
	binary.BigEndian.PutUint32(b[start-4:], uint32(len(b)-start))
}

func appendEntry(b []byte, e *Entry, version uint32, prev string) []byte {
	start := len(b)
	be := binary.BigEndian
	b = be.AppendUint32(b, e.Stat.CTime.Sec)
	b = be.AppendUint32(b, e.Stat.CTime.Nsec)
	b = be.AppendUint32(b, e.Stat.MTime.Sec)
	b = be.AppendUint32(b, e.Stat.MTime.Nsec)
	b = be.AppendUint32(b, e.Stat.Dev)
	b = be.AppendUint32(b, e.Stat.Ino)
	b = be.AppendUint32(b, uint32(e.Mode))
	b = be.AppendUint32(b, e.Stat.UID)
	b = be.AppendUint32(b, e.Stat.GID)
	b = be.AppendUint32(b, e.Stat.Size)
	b = append(b, e.ID.Raw()...)
	flags := uint16(min(len(e.Path), flagNameMask)) | uint16(e.Stage)<<flagStageShift
	if e.AssumeValid {
		flags |= flagAssumeValid
	}
	extended := version >= 3 && e.extended()
	if extended {
		flags |= flagExtended
	}
	b = be.AppendUint16(b, flags)
	if extended {
		var ext uint16
		if e.SkipWorktree {
			ext |= extFlagSkipWorktree
		}
		if e.IntentToAdd {
			ext |= extFlagIntentToAdd
		}
		b = be.AppendUint16(b, ext)
	}
	if version == 4 {
		common := 0
		for common < len(prev) && common < len(e.Path) && prev[common] == e.Path[common] {
			common++
		}
		b = appendVarint(b, uint64(len(prev)-common))
		b = append(b, e.Path[common:]...)
		return append(b, 0)
	}
	nameOffset := len(b) - start
	b = append(b, e.Path...)
	pad := entrySize(nameOffset, len(e.Path)) - nameOffset - len(e.Path)
	return append(b, make([]byte, pad)...)
}

// appendVarint encodes v in the offset encoding decodeVarint reads.
func appendVarint(b []byte, v uint64) []byte {
	var buf [10]byte
	i := len(buf) - 1
	buf[i] = byte(v & 0x7f)
	for v >>= 7; v != 0; v >>= 7 {
		v--
		i--
		buf[i] = 0x80 | byte(v&0x7f)
	}
	return append(b, buf[i:]...)
}
