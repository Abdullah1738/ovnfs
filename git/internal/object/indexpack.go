package object

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"maps"
	"math"
	"os"
	"slices"
)

// IndexPackOptions configures IndexPack.
type IndexPackOptions struct {
	// DB holds the repository's existing objects. Every object of the pack
	// that DB already stores is compared with DB's copy, and a difference
	// (a hash collision) rejects the pack. Nil skips the check.
	DB *DB
	// FixThin completes a thin pack, as git index-pack --fix-thin does:
	// ref-delta bases missing from the pack are read from DB and appended
	// to it. Without it a missing base is an error.
	FixThin bool
	// MaxObjectSize, when > 0, rejects the pack as soon as an entry's
	// declared inflated size or a delta's declared result size exceeds it,
	// before anything is inflated or allocated for it. It bounds the memory
	// a hostile pack can make delta resolution use.
	MaxObjectSize int64
}

// IndexPack reads a pack stream as received from a remote (version 2 or 3,
// trailing checksum verified), writing it to packDir as it arrives, resolves
// its deltas to name every object, and installs pack-<name>.pack with a
// version 2 index, the index last. It returns the pack's name (its hex
// checksum) and object count. Memory use is proportional to the number of
// objects and the largest delta chain, not to the pack size.
func IndexPack(r io.Reader, packDir string, f Format, opts IndexPackOptions) (name string, objects int, err error) {
	if opts.FixThin && opts.DB == nil {
		return "", 0, errors.New("index-pack: FixThin requires a DB")
	}
	if err := os.MkdirAll(packDir, 0o777); err != nil {
		return "", 0, err
	}
	file, err := createTemp(packDir, "tmp_pack_", 0o444)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if err != nil {
			file.Close()
			removeTemp(file.Name())
		}
	}()
	ix := &indexer{format: f, opts: opts, file: file, refKids: map[ID][]int{}, ofsKids: map[int][]int{}}
	sum, err := ix.receive(r)
	if err != nil {
		return "", 0, fmt.Errorf("index-pack: %w", err)
	}
	if err := ix.resolve(); err != nil {
		return "", 0, fmt.Errorf("index-pack: %w", err)
	}
	if ix.appended {
		if uint64(len(ix.entries)) > math.MaxUint32 {
			return "", 0, errors.New("index-pack: too many objects")
		}
		if sum, err = finishPack(file, f, uint32(len(ix.entries)), ix.end); err != nil {
			return "", 0, err
		}
	}
	idx := make([]indexEntry, len(ix.entries))
	for i, e := range ix.entries {
		idx[i] = indexEntry{id: e.id, offset: e.offset, crc: e.crc}
	}
	name, err = installPack(packDir, f, file, idx, sum)
	if err != nil {
		return "", 0, err
	}
	return name, len(idx), nil
}

// ixEntry is what the indexer tracks per pack entry.
type ixEntry struct {
	offset int64
	data   int64 // offset of the zlib stream
	size   int64 // inflated size: of the object, or of the delta
	crc    uint32
	typ    byte
	kind   Kind // the object's kind, 0 until resolved
	id     ID
}

type indexer struct {
	format   Format
	opts     IndexPackOptions
	file     *os.File
	entries  []ixEntry // in pack order
	refKids  map[ID][]int
	ofsKids  map[int][]int
	end      int64 // where the entries end
	appended bool  // thin-pack bases were added
	br       *bufio.Reader
}

// receive streams the pack from r into the temporary file, recording every
// entry and naming the undeltified ones, and returns the verified checksum.
func (ix *indexer) receive(r io.Reader) ([]byte, error) {
	out := bufio.NewWriterSize(ix.file, 64<<10)
	pr := &packReader{src: r, buf: make([]byte, 64<<10), out: out, hash: ix.format.New()}
	var hdr [12]byte
	if _, err := io.ReadFull(pr, hdr[:]); err != nil {
		return nil, fmt.Errorf("reading pack header: %w", err)
	}
	if !bytes.Equal(hdr[:4], []byte("PACK")) {
		return nil, errors.New("not a pack: bad signature")
	}
	if v := binary.BigEndian.Uint32(hdr[4:]); v != 2 && v != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", v)
	}
	count := binary.BigEndian.Uint32(hdr[8:])
	// The count is the sender's claim; grow towards it as entries arrive.
	ix.entries = make([]ixEntry, 0, min(count, 1<<16))
	for range count {
		if err := ix.receiveEntry(pr); err != nil {
			return nil, err
		}
	}
	pr.flush()
	ix.end = pr.off
	want := pr.hash.Sum(nil)
	got := make([]byte, len(want))
	if _, err := io.ReadFull(pr, got); err != nil {
		return nil, fmt.Errorf("reading pack checksum: %w", err)
	}
	pr.flush()
	if pr.err != nil {
		return nil, pr.err
	}
	if !bytes.Equal(got, want) {
		return nil, errors.New("pack checksum mismatch")
	}
	if err := out.Flush(); err != nil {
		return nil, err
	}
	return got, nil
}

func (ix *indexer) receiveEntry(pr *packReader) error {
	pr.flush()
	pr.crc = 0
	e := ixEntry{offset: pr.off}
	c, err := pr.ReadByte()
	if err != nil {
		return fmt.Errorf("entry at %d: %w", e.offset, unexpected(err))
	}
	e.typ = c >> 4 & 7
	size := uint64(c & 15)
	for shift := 4; c&0x80 != 0; shift += 7 {
		if shift > 60 {
			return fmt.Errorf("entry at %d: bad size", e.offset)
		}
		if c, err = pr.ReadByte(); err != nil {
			return fmt.Errorf("entry at %d: %w", e.offset, unexpected(err))
		}
		size |= uint64(c&0x7f) << shift
	}
	if size > 1<<62 {
		return fmt.Errorf("entry at %d: bad size", e.offset)
	}
	e.size = int64(size)
	if ix.opts.MaxObjectSize > 0 && e.size > ix.opts.MaxObjectSize {
		return fmt.Errorf("entry at %d: %d bytes exceeds the %d-byte object size limit", e.offset, e.size, ix.opts.MaxObjectSize)
	}
	i := len(ix.entries)
	switch e.typ {
	case byte(Commit), byte(Tree), byte(Blob), byte(Tag):
		e.kind = Kind(e.typ)
	case typeOfsDelta:
		var rel uint64
		for first := true; first || c&0x80 != 0; first = false {
			if rel > 1<<55 {
				return fmt.Errorf("entry at %d: bad base offset", e.offset)
			}
			if !first {
				rel++
			}
			if c, err = pr.ReadByte(); err != nil {
				return fmt.Errorf("entry at %d: %w", e.offset, unexpected(err))
			}
			rel = rel<<7 | uint64(c&0x7f)
		}
		base := e.offset - int64(rel)
		b, ok := slices.BinarySearchFunc(ix.entries, base, func(x ixEntry, off int64) int { return cmp.Compare(x.offset, off) })
		if rel == 0 || !ok {
			return fmt.Errorf("entry at %d: delta base offset %d is not an entry", e.offset, base)
		}
		ix.ofsKids[b] = append(ix.ofsKids[b], i)
	case typeRefDelta:
		raw := make([]byte, ix.format.Size())
		if _, err := io.ReadFull(pr, raw); err != nil {
			return fmt.Errorf("entry at %d: %w", e.offset, unexpected(err))
		}
		base, _ := IDFromRaw(ix.format, raw)
		ix.refKids[base] = append(ix.refKids[base], i)
	default:
		return fmt.Errorf("entry at %d: invalid type %d", e.offset, e.typ)
	}
	e.data = pr.pos()
	z, err := openZlib(pr)
	if err != nil {
		return fmt.Errorf("entry at %d: zlib: %w", e.offset, unexpected(err))
	}
	var sink io.Writer = io.Discard
	var h hash.Hash
	if e.kind != 0 {
		h = ix.format.New()
		h.Write(Header(e.kind, e.size))
		sink = h
	}
	remaining := e.size
	if e.kind == 0 && ix.opts.MaxObjectSize > 0 {
		// Check the delta's declared result before resolution allocates it.
		var buf [20]byte
		var n int
		n, err = io.ReadFull(z, buf[:min(e.size, int64(len(buf)))])
		if err == nil {
			var result uint64
			if _, result, _, err = deltaSizes(buf[:n]); err == nil && result > uint64(ix.opts.MaxObjectSize) {
				closeZlib(z)
				return fmt.Errorf("entry at %d: delta result of %d bytes exceeds the %d-byte object size limit", e.offset, result, ix.opts.MaxObjectSize)
			}
		}
		remaining -= int64(n)
	}
	if err == nil {
		_, err = io.CopyN(sink, z, remaining)
	}
	if err == nil {
		err = expectEnd(z)
	}
	closeZlib(z)
	if err != nil {
		return fmt.Errorf("entry at %d: inflating: %w", e.offset, unexpected(err))
	}
	if h != nil {
		e.id = ID{format: ix.format}
		h.Sum(e.id.raw[:0])
	}
	pr.flush()
	e.crc = pr.crc
	ix.entries = append(ix.entries, e)
	return pr.err
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// resolve names every deltified entry by applying it to its base, appending
// missing thin-pack bases when allowed, and checks for collisions.
func (ix *indexer) resolve() error {
	ix.br = bufio.NewReaderSize(nil, 32<<10)
	n := len(ix.entries)
	for i := range n {
		e := &ix.entries[i]
		if e.typ == typeOfsDelta || e.typ == typeRefDelta {
			continue
		}
		hasKids := len(ix.ofsKids[i]) > 0 || len(ix.refKids[e.id]) > 0
		existing := ix.opts.DB != nil && ix.opts.DB.Contains(e.id)
		if !hasKids && !existing {
			continue
		}
		data, err := ix.inflate(i)
		if err != nil {
			return err
		}
		if existing {
			if err := ix.checkCollision(e.id, e.kind, data); err != nil {
				return err
			}
		}
		if err := ix.resolveKids(i, e.id, e.kind, data); err != nil {
			return err
		}
	}
	if len(ix.refKids) > 0 && ix.opts.FixThin {
		for _, base := range slices.SortedFunc(maps.Keys(ix.refKids), ID.Compare) {
			if _, ok := ix.refKids[base]; !ok {
				continue // resolved meanwhile as the result of another delta
			}
			kind, data, err := ix.opts.DB.Read(base)
			if err != nil {
				return fmt.Errorf("thin pack base %s: %w", base, err)
			}
			i, err := ix.appendObject(base, kind, data)
			if err != nil {
				return err
			}
			if err := ix.resolveKids(i, base, kind, data); err != nil {
				return err
			}
		}
	}
	for base := range ix.refKids {
		return fmt.Errorf("delta base %s is missing from the pack", base)
	}
	for _, e := range ix.entries {
		if e.kind == 0 {
			return fmt.Errorf("delta at %d is unresolved", e.offset)
		}
	}
	return nil
}

// resolveFrame is a resolved object with deltas still to be applied to it;
// frames on the stack always have kids left.
type resolveFrame struct {
	data  []byte
	depth int   // deltas between it and its undeltified base
	kids  []int // entries still to resolve against it
}

// takeKids returns, and forgets, the entries deltified against entry i
// (named id).
func (ix *indexer) takeKids(i int, id ID) []int {
	kids := slices.Concat(ix.ofsKids[i], ix.refKids[id])
	delete(ix.ofsKids, i)
	delete(ix.refKids, id)
	return kids
}

// resolveKids resolves, depth first, every delta based on the undeltified
// entry i (named id, with the given kind and content). It iterates with an
// explicit stack so deep chains cannot exhaust the goroutine stack, drops a
// base before descending into its last delta so a linear chain holds one
// object at a time, and rejects chains longer than readers accept.
func (ix *indexer) resolveKids(i int, id ID, kind Kind, data []byte) error {
	kids := ix.takeKids(i, id)
	if len(kids) == 0 {
		return nil
	}
	stack := []resolveFrame{{data: data, kids: kids}}
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		k := top.kids[0]
		top.kids = top.kids[1:]
		base, depth := top.data, top.depth+1
		if len(top.kids) == 0 {
			stack[len(stack)-1] = resolveFrame{} // let the base be collected
			stack = stack[:len(stack)-1]
		}
		e := &ix.entries[k]
		if depth > maxDeltaChain {
			return fmt.Errorf("delta at %d: chain longer than %d", e.offset, maxDeltaChain)
		}
		delta, err := ix.inflate(k)
		if err != nil {
			return err
		}
		out, err := applyDelta(base, delta)
		if err != nil {
			return fmt.Errorf("delta at %d: %w", e.offset, err)
		}
		e.kind = kind
		e.id = Hash(ix.format, kind, out)
		if ix.opts.DB != nil && ix.opts.DB.Contains(e.id) {
			if err := ix.checkCollision(e.id, kind, out); err != nil {
				return err
			}
		}
		if kids := ix.takeKids(k, e.id); len(kids) > 0 {
			stack = append(stack, resolveFrame{data: out, depth: depth, kids: kids})
		}
	}
	return nil
}

// inflate reads entry i's data back from the temporary pack.
func (ix *indexer) inflate(i int) ([]byte, error) {
	e := ix.entries[i]
	ix.br.Reset(io.NewSectionReader(ix.file, e.data, ix.end-e.data))
	b, err := inflate(ix.br, e.size)
	if err != nil {
		return nil, fmt.Errorf("entry at %d: %w", e.offset, err)
	}
	return b, nil
}

// checkCollision fails if DB's copy of id differs from the pack's.
func (ix *indexer) checkCollision(id ID, kind Kind, data []byte) error {
	k, existing, err := ix.opts.DB.Read(id)
	if err != nil {
		return fmt.Errorf("comparing with existing object %s: %w", id, err)
	}
	if k != kind || !bytes.Equal(existing, data) {
		return fmt.Errorf("hash collision: object %s in the pack differs from the existing object", id)
	}
	return nil
}

// appendObject adds a whole object at the end of the pack, for a thin
// pack's missing base, and returns its entry index.
func (ix *indexer) appendObject(id ID, kind Kind, data []byte) (int, error) {
	w := bufio.NewWriter(io.NewOffsetWriter(ix.file, ix.end))
	cw := crcWriter{w: w}
	cw.Write(appendEntryHeader(nil, byte(kind), int64(len(data))))
	zw := zlib.NewWriter(&cw)
	zw.Write(data)
	err := zw.Close()
	if err == nil {
		err = w.Flush()
	}
	if err != nil {
		return 0, fmt.Errorf("appending thin pack base %s: %w", id, err)
	}
	ix.entries = append(ix.entries, ixEntry{offset: ix.end, crc: cw.crc, typ: byte(kind), kind: kind, id: id, size: int64(len(data))})
	ix.end += cw.n
	ix.appended = true
	return len(ix.entries) - 1, nil
}

// packReader is the pack stream's reader: it serves bytes one at a time to
// the header parser and zlib (as an io.ByteReader, so zlib never reads past
// its stream) and passes everything consumed to the temporary file, the pack
// checksum and the current entry's CRC-32.
type packReader struct {
	src  io.Reader
	buf  []byte
	r, w int // buf[r:w] is unread
	mark int // buf[mark:r] is consumed but not yet passed on
	off  int64
	out  io.Writer
	hash hash.Hash
	crc  uint32
	err  error // first write failure
}

// pos returns the stream offset of the next unread byte.
func (p *packReader) pos() int64 { return p.off + int64(p.r-p.mark) }

// flush passes consumed bytes on to the file, checksum and CRC.
func (p *packReader) flush() {
	b := p.buf[p.mark:p.r]
	if len(b) == 0 {
		return
	}
	p.hash.Write(b)
	p.crc = crc32.Update(p.crc, crc32.IEEETable, b)
	if _, err := p.out.Write(b); err != nil && p.err == nil {
		p.err = err
	}
	p.off += int64(len(b))
	p.mark = p.r
}

func (p *packReader) fill() error {
	p.flush()
	p.r, p.w, p.mark = 0, 0, 0
	for {
		n, err := p.src.Read(p.buf)
		if n > 0 {
			p.w = n
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// ReadByte implements io.ByteReader.
func (p *packReader) ReadByte() (byte, error) {
	if p.r == p.w {
		if err := p.fill(); err != nil {
			return 0, err
		}
	}
	b := p.buf[p.r]
	p.r++
	return b, nil
}

// Read implements io.Reader.
func (p *packReader) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if p.r == p.w {
		if err := p.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(b, p.buf[p.r:p.w])
	p.r += n
	return n, nil
}
