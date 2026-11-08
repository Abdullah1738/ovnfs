package object

import (
	"bufio"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
)

// indexEntry is what a pack index records about one object.
type indexEntry struct {
	id     ID
	offset int64
	crc    uint32
}

// writeIndex writes a version 2 pack index for entries, which it sorts by
// name, for the pack whose checksum is packSum.
func writeIndex(w io.Writer, f Format, entries []indexEntry, packSum []byte) error {
	slices.SortFunc(entries, func(a, b indexEntry) int { return a.id.Compare(b.id) })
	h := f.New()
	bw := bufio.NewWriter(w)
	out := io.MultiWriter(bw, h)
	var b [8]byte
	put32 := func(v uint32) {
		binary.BigEndian.PutUint32(b[:4], v)
		out.Write(b[:4])
	}
	out.Write(idxMagic)
	put32(2)
	var fanout [256]uint32
	for _, e := range entries {
		fanout[e.id.raw[0]]++
	}
	total := uint32(0)
	for _, n := range fanout {
		total += n
		put32(total)
	}
	for _, e := range entries {
		out.Write(e.id.raw[:f.Size()])
	}
	for _, e := range entries {
		put32(e.crc)
	}
	var large []int64
	for _, e := range entries {
		if e.offset < 1<<31 {
			put32(uint32(e.offset))
		} else {
			put32(1<<31 | uint32(len(large)))
			large = append(large, e.offset)
		}
	}
	for _, off := range large {
		binary.BigEndian.PutUint64(b[:], uint64(off))
		out.Write(b[:])
	}
	out.Write(packSum)
	bw.Write(h.Sum(nil))
	return bw.Flush()
}

// appendEntryHeader appends a pack entry's type-and-size header.
func appendEntryHeader(b []byte, typ byte, size int64) []byte {
	c := typ<<4 | byte(size&15)
	size >>= 4
	for size > 0 {
		b = append(b, c|0x80)
		c = byte(size & 0x7f)
		size >>= 7
	}
	return append(b, c)
}

// crcWriter forwards writes while tracking their count and CRC-32, which a
// pack index stores per entry.
type crcWriter struct {
	w   io.Writer
	crc uint32
	n   int64
}

func (c *crcWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.crc = crc32.Update(c.crc, crc32.IEEETable, p[:n])
	c.n += int64(n)
	return n, err
}

// PackWriter writes many objects at once as a new pack and its index, far
// cheaper than as many loose objects. Objects are stored whole (undeltified).
// It is not safe for concurrent use.
type PackWriter struct {
	dir     string
	format  Format
	file    *os.File
	out     *bufio.Writer
	cw      crcWriter // counts and checksums what reaches out
	zw      *zlib.Writer
	entries []indexEntry
	seen    map[ID]struct{}
	err     error // sticky: the first write failure
	done    bool
}

// NewPackWriter starts a pack of objects of format f in packDir, normally a
// repository's objects/pack, creating the directory if needed. Nothing is
// visible to readers until Commit.
func NewPackWriter(packDir string, f Format) (*PackWriter, error) {
	if err := os.MkdirAll(packDir, 0o777); err != nil {
		return nil, err
	}
	file, err := createTemp(packDir, "tmp_pack_", 0o444)
	if err != nil {
		return nil, err
	}
	w := &PackWriter{dir: packDir, format: f, file: file, seen: map[ID]struct{}{}}
	w.out = bufio.NewWriterSize(file, 64<<10)
	w.cw.w = w.out
	w.zw = zlib.NewWriter(&w.cw)
	// The count is patched in by Commit.
	w.cw.Write([]byte{'P', 'A', 'C', 'K', 0, 0, 0, 2, 0, 0, 0, 0})
	return w, nil
}

// Add appends an object and returns its name; adding an object already in
// this pack is a no-op.
func (w *PackWriter) Add(kind Kind, data []byte) (ID, error) {
	if w.done {
		return ID{}, errors.New("pack writer already finished")
	}
	if w.err != nil {
		return ID{}, w.err
	}
	id := Hash(w.format, kind, data)
	if _, ok := w.seen[id]; ok {
		return id, nil
	}
	off := w.cw.n
	w.cw.crc = 0
	w.cw.Write(appendEntryHeader(nil, byte(kind), int64(len(data))))
	w.zw.Reset(&w.cw)
	w.zw.Write(data)
	if err := w.zw.Close(); err != nil {
		w.err = fmt.Errorf("writing pack: %w", err)
		return ID{}, w.err
	}
	w.seen[id] = struct{}{}
	w.entries = append(w.entries, indexEntry{id: id, offset: off, crc: w.cw.crc})
	return id, nil
}

// Commit finishes the pack, writes its index and moves both into place, the
// index last since its appearance makes the pack visible. It returns the
// pack's name, the hex checksum in pack-<name>.pack. With no objects added
// nothing is written and the name is "".
func (w *PackWriter) Commit() (string, error) {
	if w.done {
		return "", errors.New("pack writer already finished")
	}
	defer w.Abort()
	if w.err != nil {
		return "", w.err
	}
	if len(w.entries) == 0 {
		return "", nil
	}
	if uint64(len(w.entries)) > math.MaxUint32 {
		return "", errors.New("too many objects for one pack")
	}
	if err := w.out.Flush(); err != nil {
		return "", err
	}
	sum, err := finishPack(w.file, w.format, uint32(len(w.entries)), w.cw.n)
	if err != nil {
		return "", err
	}
	return installPack(w.dir, w.format, w.file, w.entries, sum)
}

// Abort discards an unfinished pack. It is a no-op after Commit.
func (w *PackWriter) Abort() {
	if w.done {
		return
	}
	w.done = true
	w.file.Close()
	removeTemp(w.file.Name())
}

// finishPack sets the object count in the header of the pack in file, whose
// entries end at end, and appends the checksum of everything before it.
func finishPack(file *os.File, f Format, count uint32, end int64) ([]byte, error) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], count)
	if _, err := file.WriteAt(n[:], 8); err != nil {
		return nil, err
	}
	h := f.New()
	if _, err := io.Copy(h, io.NewSectionReader(file, 0, end)); err != nil {
		return nil, err
	}
	sum := h.Sum(nil)
	if _, err := file.WriteAt(sum, end); err != nil {
		return nil, err
	}
	if err := file.Truncate(end + int64(len(sum))); err != nil {
		return nil, err
	}
	return sum, nil
}

// installPack writes the index for the finished pack in the temporary
// packFile, then moves pack and index to their final names in dir. packFile
// is closed. The temporary pack is left for the caller to remove on error.
func installPack(dir string, f Format, packFile *os.File, entries []indexEntry, sum []byte) (string, error) {
	if err := packFile.Sync(); err != nil {
		return "", err
	}
	if err := packFile.Close(); err != nil {
		return "", err
	}
	idx, err := createTemp(dir, "tmp_idx_", 0o444)
	if err != nil {
		return "", err
	}
	err = writeIndex(idx, f, entries, sum)
	if err == nil {
		err = idx.Sync()
	}
	if cerr := idx.Close(); err == nil {
		err = cerr
	}
	name := hex.EncodeToString(sum)
	base := filepath.Join(dir, "pack-"+name)
	if err == nil {
		err = publish(packFile.Name(), base+".pack")
	}
	if err == nil {
		err = publish(idx.Name(), base+".idx")
	}
	if err != nil {
		removeTemp(idx.Name())
		return "", err
	}
	return name, nil
}
