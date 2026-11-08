package index

import (
	"bytes"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

const (
	signature = "DIRC"

	flagAssumeValid = 0x8000
	flagExtended    = 0x4000
	flagStageMask   = 0x3000
	flagStageShift  = 12
	flagNameMask    = 0x0fff

	extFlagReserved     = 0x8000
	extFlagSkipWorktree = 0x4000
	extFlagIntentToAdd  = 0x2000

	// statSize is the stat data before the object name: ctime, mtime (seconds
	// and nanoseconds), dev, ino, mode, uid, gid, size.
	statSize = 40
)

// ReadOptions tunes Read.
type ReadOptions struct {
	// ExpandSparse, when set, replaces sparse-directory entries by the files of
	// their trees, as git ls-files lists a sparse index by default.
	ExpandSparse TreeReader
}

// Read decodes the index file name of a repository of format f. A split index
// is merged with its shared index (sharedindex.<hash> in the same directory).
// A missing file is an error satisfying errors.Is(err, os.ErrNotExist).
func Read(name string, f object.Format, opts ReadOptions) (*Index, error) {
	data, info, err := readFile(name)
	if err != nil {
		return nil, err
	}
	idx, deferred, err := decode(data, f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if link := deferred.link; link != nil && !link.shared.IsZero() {
		sharedName := filepath.Join(filepath.Dir(name), "sharedindex."+link.shared.String())
		sharedData, _, err := readFile(sharedName)
		if err != nil {
			return nil, fmt.Errorf("read %s: shared index: %w", name, err)
		}
		shared, sharedDeferred, err := decode(sharedData, f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", sharedName, err)
		}
		if sharedDeferred.link != nil {
			return nil, fmt.Errorf("read %s: shared index is itself split", sharedName)
		}
		if !bytes.Equal(sharedData[len(sharedData)-f.Size():], link.shared.Raw()) {
			return nil, fmt.Errorf("read %s: checksum does not match its name", sharedName)
		}
		if err := idx.applySplit(link, shared); err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
	} else if link != nil {
		if err := idx.applySplit(link, &Index{}); err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
	}
	if err := checkOrder(idx.Entries); err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if deferred.fsmonitor != nil {
		if idx.FSMonitor, err = parseFSMonitor(deferred.fsmonitor, len(idx.Entries)); err != nil {
			return nil, fmt.Errorf("read %s: extension \"FSMN\": %w", name, err)
		}
	}
	idx.ModTime = info.ModTime()
	if opts.ExpandSparse != nil {
		if err := idx.ExpandSparse(opts.ExpandSparse); err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
	}
	return idx, nil
}

func readFile(name string) ([]byte, os.FileInfo, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", name, err)
	}
	return data, info, nil
}

func checkOrder(entries []Entry) error {
	for i := 1; i < len(entries); i++ {
		if compareEntry(&entries[i-1], &entries[i]) >= 0 {
			return fmt.Errorf("entries out of order at %q (stage %d)", entries[i].Path, entries[i].Stage)
		}
	}
	return nil
}

// decoder walks an index file's bytes, excluding the trailing checksum.
type decoder struct {
	f    object.Format
	data []byte
	pos  int
}

func (d *decoder) errorf(format string, args ...any) error {
	return fmt.Errorf("offset %d: %s", d.pos, fmt.Sprintf(format, args...))
}

// deferredExtensions are extensions whose bitmaps describe entries of the
// merged index, so they can only be decoded once a split index is merged.
type deferredExtensions struct {
	link      *splitLink
	fsmonitor []byte
}

// decode parses one index file. Entries of a split index are returned as
// stored (replacements, then additions), unmerged.
func decode(data []byte, f object.Format) (*Index, deferredExtensions, error) {
	hs := f.Size()
	if len(data) < 12+hs {
		return nil, deferredExtensions{}, fmt.Errorf("file too short (%d bytes)", len(data))
	}
	if string(data[:4]) != signature {
		return nil, deferredExtensions{}, fmt.Errorf("bad signature %q", data[:4])
	}
	idx := &Index{Format: f, Version: binary.BigEndian.Uint32(data[4:])}
	if idx.Version < 2 || idx.Version > 4 {
		return nil, deferredExtensions{}, fmt.Errorf("unsupported version %d", idx.Version)
	}
	body, trailer := data[:len(data)-hs], data[len(data)-hs:]
	if allZero(trailer) {
		idx.SkipHash = true
	} else {
		h := f.New()
		h.Write(body)
		if subtle.ConstantTimeCompare(h.Sum(nil), trailer) != 1 {
			return nil, deferredExtensions{}, fmt.Errorf("checksum mismatch")
		}
	}

	count := binary.BigEndian.Uint32(data[8:])
	d := &decoder{f: f, data: body, pos: 12}
	// Each entry takes at least the stat data, the name and flags; reject
	// counts the file cannot hold before allocating for them.
	if uint64(count)*uint64(statSize+hs+2) > uint64(len(body)) {
		return nil, deferredExtensions{}, fmt.Errorf("%d entries do not fit in %d bytes", count, len(body))
	}
	idx.Entries = make([]Entry, count)
	offsets := make([]uint32, count)
	prev := ""
	for i := range idx.Entries {
		offsets[i] = uint32(d.pos)
		if err := d.entry(idx.Version, prev, &idx.Entries[i]); err != nil {
			return nil, deferredExtensions{}, err
		}
		prev = idx.Entries[i].Path
	}
	deferred, err := d.extensions(idx, offsets)
	if err != nil {
		return nil, deferredExtensions{}, err
	}
	return idx, deferred, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// entry decodes the entry at d.pos into e. prev is the previous entry's path,
// the base of version 4 prefix compression.
func (d *decoder) entry(version uint32, prev string, e *Entry) error {
	start := d.pos
	hs := d.f.Size()
	fixed := statSize + hs + 2
	if len(d.data)-d.pos < fixed {
		return d.errorf("truncated entry")
	}
	b := d.data[d.pos:]
	be := binary.BigEndian
	e.Stat = Stat{
		CTime: Time{Sec: be.Uint32(b[0:]), Nsec: be.Uint32(b[4:])},
		MTime: Time{Sec: be.Uint32(b[8:]), Nsec: be.Uint32(b[12:])},
		Dev:   be.Uint32(b[16:]),
		Ino:   be.Uint32(b[20:]),
		UID:   be.Uint32(b[28:]),
		GID:   be.Uint32(b[32:]),
		Size:  be.Uint32(b[36:]),
	}
	e.Mode = object.Mode(be.Uint32(b[24:]))
	e.ID, _ = object.IDFromRaw(d.f, b[statSize:statSize+hs])
	flags := be.Uint16(b[statSize+hs:])
	d.pos += fixed
	e.AssumeValid = flags&flagAssumeValid != 0
	e.Stage = uint8((flags & flagStageMask) >> flagStageShift)
	if flags&flagExtended != 0 {
		if version < 3 {
			return d.errorf("extended flags in a version %d index", version)
		}
		if len(d.data)-d.pos < 2 {
			return d.errorf("truncated entry")
		}
		ext := be.Uint16(d.data[d.pos:])
		d.pos += 2
		if ext&^(extFlagSkipWorktree|extFlagIntentToAdd) != 0 {
			return d.errorf("unknown extended flags %#04x", ext)
		}
		e.SkipWorktree = ext&extFlagSkipWorktree != 0
		e.IntentToAdd = ext&extFlagIntentToAdd != 0
	}

	if version == 4 {
		strip, err := d.varint()
		if err != nil {
			return err
		}
		if strip > uint64(len(prev)) {
			return d.errorf("name strips %d bytes from a %d-byte name", strip, len(prev))
		}
		end := bytes.IndexByte(d.data[d.pos:], 0)
		if end < 0 {
			return d.errorf("unterminated name")
		}
		e.Path = prev[:len(prev)-int(strip)] + string(d.data[d.pos:d.pos+end])
		d.pos += end + 1
		return nil
	}

	nameLen := int(flags & flagNameMask)
	var end int
	if nameLen < flagNameMask {
		end = d.pos + nameLen
		if end >= len(d.data) || d.data[end] != 0 {
			return d.errorf("bad name length %d", nameLen)
		}
	} else {
		i := bytes.IndexByte(d.data[d.pos:], 0)
		if i < 0 {
			return d.errorf("unterminated name")
		}
		end = d.pos + i
	}
	e.Path = string(d.data[d.pos:end])
	d.pos = start + entrySize(d.pos-start, len(e.Path))
	if d.pos > len(d.data) {
		return d.errorf("truncated entry padding")
	}
	return nil
}

// entrySize is the padded size of a version 2/3 entry whose name starts
// nameOffset bytes in and is nameLen bytes long: 1-8 NULs round it to a
// multiple of eight.
func entrySize(nameOffset, nameLen int) int {
	return (nameOffset + nameLen + 8) &^ 7
}

// varint decodes the offset encoding of gitformat-pack (OFS_DELTA), used by
// version 4 names and the UNTR extension.
func (d *decoder) varint() (uint64, error) {
	v, n := decodeVarint(d.data[d.pos:])
	if n == 0 {
		return 0, d.errorf("bad variable-width integer")
	}
	d.pos += n
	return v, nil
}

func decodeVarint(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	c := b[0]
	v := uint64(c & 0x7f)
	n := 1
	for c&0x80 != 0 {
		if n >= len(b) || n > 9 {
			return 0, 0
		}
		c = b[n]
		n++
		v = (v+1)<<7 | uint64(c&0x7f)
	}
	return v, n
}

// extensions decodes the extensions after the entries into idx and verifies
// EOIE and IEOT against the entries' offsets.
func (d *decoder) extensions(idx *Index, offsets []uint32) (deferredExtensions, error) {
	entriesEnd := d.pos
	var deferred deferredExtensions
	sizes := d.f.New() // sizes hash for EOIE
	for d.pos < len(d.data) {
		if len(d.data)-d.pos < 8 {
			return deferredExtensions{}, d.errorf("truncated extension header")
		}
		sig := string(d.data[d.pos : d.pos+4])
		size := binary.BigEndian.Uint32(d.data[d.pos+4:])
		header := d.data[d.pos : d.pos+8]
		d.pos += 8
		if uint64(len(d.data)-d.pos) < uint64(size) {
			return deferredExtensions{}, d.errorf("extension %q overruns the file", sig)
		}
		ext := d.data[d.pos : d.pos+int(size)]
		var err error
		switch sig {
		case "TREE":
			idx.CacheTree, err = parseCacheTree(ext, d.f)
		case "REUC":
			idx.ResolveUndo, err = parseResolveUndo(ext, d.f)
		case "link":
			deferred.link, err = parseLink(ext, d.f)
		case "UNTR":
			idx.Untracked, err = parseUntracked(ext, d.f)
		case "FSMN":
			deferred.fsmonitor = ext
		case "sdir":
			// Marks a sparse index; sparse-directory entries identify themselves.
		case "IEOT":
			err = checkIEOT(ext, offsets)
		case "EOIE":
			if d.pos+int(size) != len(d.data) {
				err = fmt.Errorf("not the last extension")
			} else {
				err = checkEOIE(ext, d.f, entriesEnd, sizes.Sum(nil))
			}
		default:
			if sig[0] < 'A' || sig[0] > 'Z' {
				return deferredExtensions{}, d.errorf("unsupported required extension %q", sig)
			}
		}
		if err != nil {
			return deferredExtensions{}, d.errorf("extension %q: %v", sig, err)
		}
		sizes.Write(header)
		d.pos += int(size)
	}
	return deferred, nil
}

// checkEOIE verifies the End of Index Entry extension: the offset of the
// first extension and the hash over the preceding extensions' headers.
func checkEOIE(ext []byte, f object.Format, entriesEnd int, sizes []byte) error {
	if len(ext) != 4+f.Size() {
		return fmt.Errorf("size %d", len(ext))
	}
	if off := binary.BigEndian.Uint32(ext); int(off) != entriesEnd {
		return fmt.Errorf("records entries ending at %d, not %d", off, entriesEnd)
	}
	if !bytes.Equal(ext[4:], sizes) {
		return fmt.Errorf("extension hash mismatch")
	}
	return nil
}

// checkIEOT verifies the Index Entry Offset Table against where entries were
// actually found.
func checkIEOT(ext []byte, offsets []uint32) error {
	if len(ext) < 4 || (len(ext)-4)%8 != 0 {
		return fmt.Errorf("size %d", len(ext))
	}
	if v := binary.BigEndian.Uint32(ext); v != 1 {
		return fmt.Errorf("unsupported version %d", v)
	}
	next := 0
	for block := range slices.Chunk(ext[4:], 8) {
		off, n := binary.BigEndian.Uint32(block), int(binary.BigEndian.Uint32(block[4:]))
		if next >= len(offsets) || offsets[next] != off || n > len(offsets)-next {
			return fmt.Errorf("block at offset %d does not start an entry", off)
		}
		next += n
	}
	if next != len(offsets) {
		return fmt.Errorf("blocks cover %d of %d entries", next, len(offsets))
	}
	return nil
}
