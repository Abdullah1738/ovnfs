package index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// UntrackedCache is the UNTR extension: untracked files per directory and the
// data needed to tell whether that list is still current. Write drops it; git
// rebuilds it on the next status.
type UntrackedCache struct {
	Ident         []string // environments (location, system) the cache is valid in
	InfoExclude   ExcludeFile
	ExcludesFile  ExcludeFile // core.excludesFile
	DirFlags      uint32
	ExcludePerDir string        // usually ".gitignore"
	Root          *UntrackedDir // nil when the cache records no directory
}

// ExcludeFile is the recorded state of an exclude file; a zero ID means the
// file did not exist.
type ExcludeFile struct {
	Stat Stat
	ID   object.ID
}

// UntrackedDir is a directory block of the untracked cache.
type UntrackedDir struct {
	Name      string
	Untracked []string
	Dirs      []*UntrackedDir
	Valid     bool // the untracked list is current; Stat is recorded
	CheckOnly bool
	Stat      Stat      // the directory's stat data when Valid
	ExcludeID object.ID // hash of the directory's per-directory exclude file, when recorded
}

var errUntracked = errors.New("malformed untracked cache")

// untrackedStatSize is UNTR's stat record: an index entry's stat data
// without the mode word.
const untrackedStatSize = 36

func decodeUntrackedStat(b []byte) Stat {
	be := binary.BigEndian
	return Stat{
		CTime: Time{Sec: be.Uint32(b[0:]), Nsec: be.Uint32(b[4:])},
		MTime: Time{Sec: be.Uint32(b[8:]), Nsec: be.Uint32(b[12:])},
		Dev:   be.Uint32(b[16:]),
		Ino:   be.Uint32(b[20:]),
		UID:   be.Uint32(b[24:]),
		GID:   be.Uint32(b[28:]),
		Size:  be.Uint32(b[32:]),
	}
}

// untrackedReader consumes an UNTR payload.
type untrackedReader struct {
	b   []byte
	err error
}

func (r *untrackedReader) take(n int) []byte {
	if r.err != nil || n < 0 || len(r.b) < n {
		r.err = errUntracked
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *untrackedReader) varint() int {
	if r.err != nil {
		return 0
	}
	v, n := decodeVarint(r.b)
	if n == 0 || v > uint64(len(r.b)) {
		// Every count bounds a number of items each taking at least a byte.
		r.err = errUntracked
		return 0
	}
	r.b = r.b[n:]
	return int(v)
}

func (r *untrackedReader) str() string {
	if r.err != nil {
		return ""
	}
	nul := bytes.IndexByte(r.b, 0)
	if nul < 0 {
		r.err = errUntracked
		return ""
	}
	s := string(r.b[:nul])
	r.b = r.b[nul+1:]
	return s
}

func (r *untrackedReader) bitmap(maxBits int) bitmap {
	if r.err != nil {
		return bitmap{}
	}
	bm, rest, err := decodeEWAH(r.b, maxBits)
	if err != nil {
		r.err = err
		return bitmap{}
	}
	r.b = rest
	return bm
}

func parseUntracked(b []byte, f object.Format) (*UntrackedCache, error) {
	r := &untrackedReader{b: b}
	u := &UntrackedCache{}
	idents := &untrackedReader{b: r.take(r.varint())}
	for r.err == nil && idents.err == nil && len(idents.b) > 0 {
		u.Ident = append(u.Ident, idents.str())
	}
	if idents.err != nil {
		return nil, idents.err
	}
	if stat := r.take(2 * untrackedStatSize); r.err == nil {
		u.InfoExclude.Stat = decodeUntrackedStat(stat)
		u.ExcludesFile.Stat = decodeUntrackedStat(stat[untrackedStatSize:])
	}
	if flags := r.take(4); r.err == nil {
		u.DirFlags = binary.BigEndian.Uint32(flags)
	}
	if ids := r.take(2 * f.Size()); r.err == nil {
		u.InfoExclude.ID, _ = object.IDFromRaw(f, ids[:f.Size()])
		u.ExcludesFile.ID, _ = object.IDFromRaw(f, ids[f.Size():])
	}
	u.ExcludePerDir = r.str()
	count := r.varint()
	if r.err != nil {
		return nil, r.err
	}
	if count == 0 {
		if len(r.b) > 1 || (len(r.b) == 1 && r.b[0] != 0) {
			return nil, errUntracked
		}
		return u, nil
	}

	dirs := make([]*UntrackedDir, 0, count)
	var readDir func(depth int) *UntrackedDir
	readDir = func(depth int) *UntrackedDir {
		if depth > maxCacheTreeDepth {
			r.err = errUntracked
		}
		untracked, subdirs := r.varint(), r.varint()
		d := &UntrackedDir{Name: r.str()}
		dirs = append(dirs, d)
		for range untracked {
			if r.err != nil {
				return nil
			}
			d.Untracked = append(d.Untracked, r.str())
		}
		for range subdirs {
			if r.err != nil || len(dirs) >= count {
				r.err = errUntracked
				return nil
			}
			d.Dirs = append(d.Dirs, readDir(depth+1))
		}
		return d
	}
	u.Root = readDir(0)
	if r.err == nil && len(dirs) != count {
		return nil, fmt.Errorf("%w: %d of %d directory blocks", errUntracked, len(dirs), count)
	}
	valid, checkOnly, hashed := r.bitmap(len(dirs)), r.bitmap(len(dirs)), r.bitmap(len(dirs))
	if r.err != nil {
		return nil, r.err
	}
	// git writes a stat record for each valid directory (not, as
	// gitformat-index suggests, for each bit of the third bitmap), then a
	// hash for each bit of the third bitmap.
	for i := range valid.ones() {
		if i >= len(dirs) {
			return nil, errUntracked
		}
		dirs[i].Valid = true
		if stat := r.take(untrackedStatSize); r.err == nil {
			dirs[i].Stat = decodeUntrackedStat(stat)
		}
	}
	for i := range checkOnly.ones() {
		if i >= len(dirs) {
			return nil, errUntracked
		}
		dirs[i].CheckOnly = true
	}
	for i := range hashed.ones() {
		if i >= len(dirs) {
			return nil, errUntracked
		}
		if id := r.take(f.Size()); r.err == nil {
			dirs[i].ExcludeID, _ = object.IDFromRaw(f, id)
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	if len(r.b) != 1 || r.b[0] != 0 {
		return nil, fmt.Errorf("%w: bad terminator", errUntracked)
	}
	return u, nil
}

// FSMonitor is the FSMN extension: which entries the core.fsmonitor hook has
// not vouched for since a point in time (version 1) or an opaque token
// (version 2). Write drops it.
type FSMonitor struct {
	Version uint32
	Since   time.Time // version 1
	Token   string    // version 2
	// Dirty lists the positions in Entries of entries not marked
	// fsmonitor-valid.
	Dirty []int
}

// parseFSMonitor decodes an FSMN payload whose bitmap describes up to
// entries index entries.
func parseFSMonitor(b []byte, entries int) (*FSMonitor, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("size %d", len(b))
	}
	m := &FSMonitor{Version: binary.BigEndian.Uint32(b)}
	b = b[4:]
	switch m.Version {
	case 1:
		if len(b) < 8 {
			return nil, fmt.Errorf("size %d", len(b))
		}
		m.Since = time.Unix(0, int64(binary.BigEndian.Uint64(b)))
		b = b[8:]
	case 2:
		nul := bytes.IndexByte(b, 0)
		if nul < 0 {
			return nil, fmt.Errorf("unterminated token")
		}
		m.Token = string(b[:nul])
		b = b[nul+1:]
	default:
		return nil, fmt.Errorf("unsupported version %d", m.Version)
	}
	if len(b) < 4 {
		return nil, fmt.Errorf("truncated bitmap size")
	}
	size := binary.BigEndian.Uint32(b)
	b = b[4:]
	if uint64(size) != uint64(len(b)) {
		return nil, fmt.Errorf("bitmap size %d, %d bytes left", size, len(b))
	}
	bm, rest, err := decodeEWAH(b, entries)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("%d trailing bytes", len(rest))
	}
	for i := range bm.ones() {
		m.Dirty = append(m.Dirty, i)
	}
	return m, nil
}
