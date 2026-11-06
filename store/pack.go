package store

import (
	"bufio"
	"cmp"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// Pack frame layout, integers little-endian:
//
//	0  CRC-32C (Castagnoli) of bytes 4 to the end of the frame
//	4  object id, 20 raw bytes
//	24 flags (frameZstd, frameDelta)
//	25 3 zero bytes
//	28 content length, uint32
//	32 payload length, uint32
//	36 freshness when appended, int64 Unix nanoseconds
//	44 payload: the content, zstd-compressed when frameZstd is set; with
//	   frameDelta, the raw id of the base object followed by the content
//	   compressed with the base's content as zstd raw dictionary
const (
	frameHeader = 44
	// frameZstd marks a payload compressed as one zstd frame.
	frameZstd = 1 << 0
	// frameDelta marks a payload compressed against a base object (see delta.go).
	frameDelta = 1 << 1
)

// Pack index (<n>.idx) layout: one idxRecord-byte record per frame (id,
// offset, payload length, content length, flags, 3 zero bytes, freshness,
// delta base id or zeros), then a trailer holding the pack's length
// (uint64), the record count (uint32) and the CRC-32C of everything before it.
const (
	idxRecord  = 64
	idxTrailer = 16
)

// packLimit is the length past which the active pack is sealed and a new one
// started; it bounds both the scan at Open and the bytes Sweep copies to
// rewrite one pack.
const packLimit = 32 << 20

// castagnoli is the CRC-32C table, hardware-accelerated on amd64 and arm64.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// errCorrupt reports a frame whose checksum or header does not match its index entry.
var errCorrupt = errors.New("corrupt pack frame")

// encoder compresses pack payloads; EncodeAll is safe for concurrent use.
var encoder = sync.OnceValue(func() *zstd.Encoder {
	e, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderCRC(false), zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)))
	if err != nil {
		panic(err) // constant options
	}
	return e
})

// decoder decompresses pack payloads; DecodeAll is safe for concurrent use and,
// with the cap limit, never produces more than the content length a frame declares.
var decoder = sync.OnceValue(func() *zstd.Decoder {
	d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecodeAllCapLimit(true))
	if err != nil {
		panic(err) // constant options
	}
	return d
})

// rawID is an object id as 20 raw bytes, the pack index key.
type rawID [20]byte

// rawOf decodes a valid id.
func rawOf(id OID) rawID {
	var r rawID
	for i := range r {
		r[i] = unhex(id[2*i])<<4 | unhex(id[2*i+1])
	}
	return r
}

// unhex decodes one lowercase hex digit.
func unhex(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'a' + 10
}

// oid encodes r as an object id.
func (r rawID) oid() OID { return OID(hex.EncodeToString(r[:])) }

// packed locates one object in a pack.
type packed struct {
	pack   uint32
	off    uint32 // of the frame header
	stored uint32 // payload length
	raw    uint32 // content length
	flags  uint8
	// at is the object's freshness for Sweep, Unix nanoseconds: when it was
	// appended or last stored again, whichever is later.
	at int64
}

// encodeFrame returns data as a complete frame for id, fresh at at; the
// payload is zstd-compressed when that makes it smaller.
func encodeFrame(id rawID, data []byte, at int64) []byte {
	buf := make([]byte, frameHeader, frameHeader+len(data))
	buf = encoder().EncodeAll(data, buf)
	flags := uint8(frameZstd)
	if len(buf)-frameHeader >= len(data) {
		buf = append(buf[:frameHeader], data...)
		flags = 0
	}
	sealFrame(buf, id, flags, len(data), at)
	return buf
}

// sealFrame fills in the header of frame, whose payload follows it, and its checksum.
func sealFrame(frame []byte, id rawID, flags uint8, raw int, at int64) {
	copy(frame[4:24], id[:])
	frame[24] = flags
	binary.LittleEndian.PutUint32(frame[28:], uint32(raw))
	binary.LittleEndian.PutUint32(frame[32:], uint32(len(frame)-frameHeader))
	binary.LittleEndian.PutUint64(frame[36:], uint64(at))
	binary.LittleEndian.PutUint32(frame, crc32.Checksum(frame[4:], castagnoli))
}

// parseHeader decodes a frame header, reporting whether its fields are
// consistent; the payload checksum is not verified.
func parseHeader(h []byte) (rawID, packed, bool) {
	var r rawID
	copy(r[:], h[4:24])
	e := packed{
		flags:  h[24],
		raw:    binary.LittleEndian.Uint32(h[28:]),
		stored: binary.LittleEndian.Uint32(h[32:]),
		at:     int64(binary.LittleEndian.Uint64(h[36:])),
	}
	ok := h[25] == 0 && h[26] == 0 && h[27] == 0 && e.stored <= cloneFile
	switch e.flags {
	case 0:
		ok = ok && e.stored == e.raw
	case frameZstd:
		ok = ok && e.raw <= cloneFile
	case frameZstd | frameDelta:
		ok = ok && e.raw <= deltaMax && e.stored >= uint32(len(rawID{}))
	default:
		ok = false
	}
	return r, e, ok
}

// deltaBase returns the base id of a delta frame's payload.
func deltaBase(frame []byte) rawID {
	var base rawID
	copy(base[:], frame[frameHeader:])
	return base
}

// checkFrame verifies frame, read at e's location, is intact and holds id as e describes.
func checkFrame(id rawID, e packed, frame []byte) error {
	if len(frame) != frameHeader+int(e.stored) || binary.LittleEndian.Uint32(frame) != crc32.Checksum(frame[4:], castagnoli) {
		return errCorrupt
	}
	r, h, ok := parseHeader(frame)
	if !ok || r != id || h.flags != e.flags || h.raw != e.raw || h.stored != e.stored {
		return errCorrupt
	}
	return nil
}

// decodeFrame returns the content of a checked frame other than a delta in a new slice.
func decodeFrame(e packed, frame []byte) ([]byte, error) {
	payload := frame[frameHeader:]
	if e.flags&frameZstd == 0 {
		return payload, nil
	}
	data, err := decoder().DecodeAll(payload, make([]byte, 0, e.raw))
	if err != nil || len(data) != int(e.raw) {
		return nil, errCorrupt
	}
	return data, nil
}

// packFile is one open pack.
type packFile struct {
	n uint32
	f *os.File
	// refs counts the set's listing plus in-flight reads; the file closes at zero,
	// so a read racing Sweep's rewrite finishes on the old pack.
	refs atomic.Int32
}

// release drops one reference, closing the file with the last.
func (pf *packFile) release() {
	if pf.refs.Add(-1) == 0 {
		pf.f.Close()
	}
}

// packSet holds the store's packs and the in-memory index of their objects.
type packSet struct {
	dir   string
	limit int64 // packLimit; smaller in tests
	// mu guards index, bases and files; readers hold it only to look up.
	mu    sync.RWMutex
	index map[rawID]packed
	bases map[rawID]rawID // base of each indexed delta object
	files map[uint32]*packFile
	// appendMu serializes appends, sealing and rewrites and guards the fields below.
	appendMu sync.Mutex
	active   *packFile // the pack appends go to; nil until the next append creates one
	end      int64     // active's length
	next     uint32    // number of the next new pack
}

// packPath is the file of pack n.
func (p *packSet) packPath(n uint32) string {
	return filepath.Join(p.dir, strconv.FormatUint(uint64(n), 10)+".pack")
}

// idxPath is the index file of pack n.
func (p *packSet) idxPath(n uint32) string {
	return filepath.Join(p.dir, strconv.FormatUint(uint64(n), 10)+".idx")
}

// openPacks loads the packs under dir (created if needed): a sealed pack from
// its index, any other by scanning its frames, cutting it at the first
// incomplete or corrupt one (a torn tail). The newest unsealed pack with room
// becomes the active one; other unsealed packs are sealed.
func openPacks(dir string) (*packSet, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := &packSet{dir: dir, limit: packLimit, index: map[rawID]packed{}, bases: map[rawID]rawID{}, files: map[uint32]*packFile{}}
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var packs []uint32
	idxs := map[uint32]bool{}
	for _, e := range names {
		base, ext, _ := strings.Cut(e.Name(), ".")
		n, err := strconv.ParseUint(base, 10, 32)
		switch {
		case err == nil && ext == "pack":
			packs = append(packs, uint32(n))
		case err == nil && ext == "idx":
			idxs[uint32(n)] = true
		default:
			// Leftover index staging files and anything else unknown.
			removeFile(filepath.Join(dir, e.Name()))
		}
	}
	slices.Sort(packs)
	if len(packs) > 0 {
		p.next = packs[len(packs)-1] + 1
	}
	var unsealed []*packFile
	var ends []int64
	for _, n := range packs {
		delete(idxs, n)
		f, err := hostfile.OpenFile(p.packPath(n), os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		pf := &packFile{n: n, f: f}
		pf.refs.Store(1)
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		if p.loadIdx(n, info.Size()) {
			p.files[n] = pf
			continue
		}
		removeFile(p.idxPath(n))
		end, err := p.scan(pf)
		if err == nil && end < info.Size() {
			err = f.Truncate(end)
		}
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("pack %d: %w", n, err)
		}
		if end == 0 {
			f.Close()
			removeFile(p.packPath(n))
			continue
		}
		p.files[n] = pf
		unsealed, ends = append(unsealed, pf), append(ends, end)
	}
	for n := range idxs {
		removeFile(p.idxPath(n))
	}
	for i, pf := range unsealed {
		p.active, p.end = pf, ends[i]
		if i == len(unsealed)-1 && ends[i] < p.limit {
			break
		}
		if err := p.seal(true); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// scan indexes the frames of pf from its start, returning the length of its
// intact prefix; objects already indexed from another pack keep that entry.
func (p *packSet) scan(pf *packFile) (int64, error) {
	r := bufio.NewReaderSize(io.NewSectionReader(pf.f, 0, 1<<62), 1<<20)
	var off int64
	frame := make([]byte, frameHeader, frameHeader+cloneFile)
	for {
		if _, err := io.ReadFull(r, frame[:frameHeader]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return off, nil
			}
			return 0, err
		}
		id, e, ok := parseHeader(frame)
		if !ok {
			return off, nil
		}
		frame = frame[:frameHeader+int(e.stored)]
		if _, err := io.ReadFull(r, frame[frameHeader:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return off, nil
			}
			return 0, err
		}
		if binary.LittleEndian.Uint32(frame) != crc32.Checksum(frame[4:], castagnoli) {
			return off, nil
		}
		e.pack, e.off = pf.n, uint32(off)
		if _, dup := p.index[id]; !dup {
			p.index[id] = e
			if e.flags&frameDelta != 0 {
				p.bases[id] = deltaBase(frame)
			}
		}
		off += int64(len(frame))
	}
}

// loadIdx indexes pack n of length size from its index file, reporting
// whether that file was intact and matched the pack.
func (p *packSet) loadIdx(n uint32, size int64) bool {
	data, err := os.ReadFile(p.idxPath(n))
	if err != nil || len(data) < idxTrailer || (len(data)-idxTrailer)%idxRecord != 0 {
		return false
	}
	body, trailer := data[:len(data)-idxTrailer], data[len(data)-idxTrailer:]
	if binary.LittleEndian.Uint64(trailer) != uint64(size) ||
		int(binary.LittleEndian.Uint32(trailer[8:])) != len(body)/idxRecord ||
		binary.LittleEndian.Uint32(trailer[12:]) != crc32.Checksum(body, castagnoli) {
		return false
	}
	for rec := range slices.Chunk(body, idxRecord) {
		var id rawID
		copy(id[:], rec)
		e := packed{
			pack:   n,
			off:    binary.LittleEndian.Uint32(rec[20:]),
			stored: binary.LittleEndian.Uint32(rec[24:]),
			raw:    binary.LittleEndian.Uint32(rec[28:]),
			flags:  rec[32],
			at:     int64(binary.LittleEndian.Uint64(rec[36:])),
		}
		if _, dup := p.index[id]; !dup {
			p.index[id] = e
			if e.flags&frameDelta != 0 {
				var base rawID
				copy(base[:], rec[44:])
				p.bases[id] = base
			}
		}
	}
	return true
}

// writeIdx writes the index file of pack n, of length size, from the in-memory index.
func (p *packSet) writeIdx(n uint32, size int64) error {
	p.mu.RLock()
	var body []byte
	for id, e := range p.index {
		if e.pack != n {
			continue
		}
		var rec [idxRecord]byte
		copy(rec[:], id[:])
		binary.LittleEndian.PutUint32(rec[20:], e.off)
		binary.LittleEndian.PutUint32(rec[24:], e.stored)
		binary.LittleEndian.PutUint32(rec[28:], e.raw)
		rec[32] = e.flags
		binary.LittleEndian.PutUint64(rec[36:], uint64(e.at))
		if base, ok := p.bases[id]; ok {
			copy(rec[44:], base[:])
		}
		body = append(body, rec[:]...)
	}
	p.mu.RUnlock()
	var trailer [idxTrailer]byte
	binary.LittleEndian.PutUint64(trailer[:], uint64(size))
	binary.LittleEndian.PutUint32(trailer[8:], uint32(len(body)/idxRecord))
	binary.LittleEndian.PutUint32(trailer[12:], crc32.Checksum(body, castagnoli))
	tmp := p.idxPath(n) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(body, trailer[:]...))
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, p.idxPath(n))
	}
	if err != nil {
		removeFile(tmp)
	}
	return err
}

// seal ends appends to the active pack: it is made durable and, when indexed
// is set, given an index file so Open need not scan it. A failed index write
// only costs that scan, so it does not fail the seal. p.appendMu must be held.
func (p *packSet) seal(indexed bool) error {
	pf := p.active
	if pf == nil {
		return nil
	}
	if err := pf.f.Sync(); err != nil {
		return err
	}
	if indexed {
		p.writeIdx(pf.n, p.end)
	}
	p.active, p.end = nil, 0
	return nil
}

// reserve makes the active pack able to take n more bytes, sealing a full one
// and starting a new one as needed; p.appendMu must be held.
func (p *packSet) reserve(n int) error {
	if p.active != nil && (p.end == 0 || p.end+int64(n) <= p.limit) {
		return nil
	}
	if err := p.seal(true); err != nil {
		return err
	}
	num := p.next
	f, err := hostfile.OpenFile(p.packPath(num), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	p.next++
	pf := &packFile{n: num, f: f}
	pf.refs.Store(1)
	p.mu.Lock()
	p.files[num] = pf
	p.mu.Unlock()
	p.active, p.end = pf, 0
	return nil
}

// write appends frame to the active pack, returning where it went; a failed
// write is cut off again. p.appendMu must be held.
func (p *packSet) write(frame []byte) (uint32, uint32, error) {
	if err := p.reserve(len(frame)); err != nil {
		return 0, 0, err
	}
	off := p.end
	if _, err := p.active.f.WriteAt(frame, off); err != nil {
		p.active.f.Truncate(off)
		return 0, 0, err
	}
	p.end += int64(len(frame))
	return p.active.n, uint32(off), nil
}

// add appends frame, encoded by encodeFrame, unless its object is already
// packed, which is then refreshed instead; it reports whether it was.
func (p *packSet) add(frame []byte) (bool, error) {
	id, e, _ := parseHeader(frame)
	p.appendMu.Lock()
	defer p.appendMu.Unlock()
	if p.touch(id, e.at) {
		return true, nil
	}
	var err error
	if e.pack, e.off, err = p.write(frame); err != nil {
		return false, err
	}
	p.mu.Lock()
	p.index[id] = e
	if e.flags&frameDelta != 0 {
		p.bases[id] = deltaBase(frame)
	}
	p.mu.Unlock()
	return false, nil
}

// depth returns the number of deltas to decode to read packed object id.
func (p *packSet) depth(id rawID) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for {
		base, ok := p.bases[id]
		if !ok {
			return n
		}
		id = base
		n++
	}
}

// keepBases adds to live the delta bases, transitively, of every packed
// object a Sweep with cutoff before keeps (live, or fresh at or after before),
// so no kept delta loses its base.
func (p *packSet) keepBases(live map[OID]struct{}, before int64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for id, base := range p.bases {
		if p.index[id].at < before {
			if _, ok := live[id.oid()]; !ok {
				continue
			}
		}
		for {
			live[base.oid()] = struct{}{}
			next, ok := p.bases[base]
			if !ok {
				break
			}
			base = next
		}
	}
}

// lookup returns the location of packed object id.
func (p *packSet) lookup(id rawID) (packed, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.index[id]
	return e, ok
}

// touch raises packed object id's freshness to at, reporting whether it is packed.
func (p *packSet) touch(id rawID, at int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.index[id]
	if ok && e.at < at {
		e.at = at
		p.index[id] = e
	}
	return ok
}

// frame reads and verifies the frame of packed object id; ok is false when it is not packed.
func (p *packSet) frame(id rawID) (packed, []byte, bool, error) {
	p.mu.RLock()
	e, ok := p.index[id]
	var pf *packFile
	if ok {
		pf = p.files[e.pack]
		pf.refs.Add(1)
	}
	p.mu.RUnlock()
	if !ok {
		return packed{}, nil, false, nil
	}
	frame := make([]byte, frameHeader+int(e.stored))
	_, err := pf.f.ReadAt(frame, int64(e.off))
	pf.release()
	if err == nil {
		err = checkFrame(id, e, frame)
	}
	if err != nil {
		return e, nil, true, fmt.Errorf("pack %d offset %d: %w", e.pack, e.off, err)
	}
	return e, frame, true, nil
}

// rewriteShare is the inverse of the share of a pack's bytes that must be
// garbage before Sweep rewrites it: below a quarter, dead objects stay in
// place (and stored) until later sweeps have found enough of them.
const rewriteShare = 4

// removals lists the packed objects a Sweep removes: those outside live whose
// freshness predates before, but only from packs where they and frames no
// longer indexed make up at least 1/rewriteShare of the pack. Dead objects
// left in other packs stay stored, so their delta bases join live.
func (p *packSet) removals(live map[OID]struct{}, before int64) ([]rawID, error) {
	p.appendMu.Lock()
	defer p.appendMu.Unlock()
	p.mu.RLock()
	defer p.mu.RUnlock()
	dead := map[rawID]packed{}
	indexed := map[uint32]int64{}
	var hexID [40]byte
	for id, e := range p.index {
		indexed[e.pack] += frameHeader + int64(e.stored)
		if e.at >= before {
			continue
		}
		hex.Encode(hexID[:], id[:])
		if _, ok := live[OID(hexID[:])]; !ok {
			dead[id] = e
		}
	}
	sizes := map[uint32]int64{}
	for {
		deadBytes := map[uint32]int64{}
		for _, e := range dead {
			deadBytes[e.pack] += frameHeader + int64(e.stored)
		}
		victims := map[uint32]bool{}
		for n, d := range deadBytes {
			size, ok := sizes[n]
			if !ok {
				if p.active != nil && p.active.n == n {
					size = p.end
				} else {
					info, err := p.files[n].f.Stat()
					if err != nil {
						return nil, err
					}
					size = info.Size()
				}
				sizes[n] = size
			}
			victims[n] = (size-(indexed[n]-d))*rewriteShare >= size
		}
		// A dead delta left in place keeps its base chain, which may make
		// other packs hold less garbage: repeat until nothing changes.
		changed := false
		for id, e := range dead {
			if victims[e.pack] {
				continue
			}
			for base, ok := p.bases[id]; ok; base, ok = p.bases[base] {
				live[base.oid()] = struct{}{}
				if _, isDead := dead[base]; isDead {
					delete(dead, base)
					changed = true
				}
			}
		}
		if !changed {
			var remove []rawID
			for id, e := range dead {
				if victims[e.pack] {
					remove = append(remove, id)
				}
			}
			return remove, nil
		}
	}
}

// remove drops dead from the index and rewrites every pack that held one:
// its other frames are copied verbatim (still compressed, keeping their
// freshness) to the active pack, made durable, and only then is the old pack
// deleted. Reads in flight on an old pack finish on its open file.
func (p *packSet) remove(dead []rawID) error {
	p.appendMu.Lock()
	defer p.appendMu.Unlock()
	victims := map[uint32]bool{}
	p.mu.Lock()
	for _, id := range dead {
		if e, ok := p.index[id]; ok {
			victims[e.pack] = true
			delete(p.index, id)
			delete(p.bases, id)
		}
	}
	p.mu.Unlock()
	if p.active != nil && victims[p.active.n] {
		if err := p.seal(false); err != nil {
			return err
		}
	}
	for _, n := range slices.Sorted(maps.Keys(victims)) {
		if err := p.rewrite(n); err != nil {
			return err
		}
	}
	if p.active != nil {
		if err := p.active.f.Sync(); err != nil {
			return err
		}
	}
	for n := range victims {
		p.mu.Lock()
		pf := p.files[n]
		delete(p.files, n)
		p.mu.Unlock()
		removeFile(p.idxPath(n))
		if err := hostfile.Remove(p.packPath(n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		pf.release()
	}
	return nil
}

// rewrite copies the indexed frames of sealed pack n to the active pack, in
// their order, repointing the index; p.appendMu must be held.
func (p *packSet) rewrite(n uint32) error {
	type moved struct {
		id rawID
		e  packed
	}
	var frames []moved
	p.mu.RLock()
	pf := p.files[n]
	for id, e := range p.index {
		if e.pack == n {
			frames = append(frames, moved{id, e})
		}
	}
	p.mu.RUnlock()
	slices.SortFunc(frames, func(a, b moved) int { return cmp.Compare(a.e.off, b.e.off) })
	buf := make([]byte, 0, frameHeader+cloneFile)
	for _, m := range frames {
		buf = buf[:frameHeader+int(m.e.stored)]
		if _, err := pf.f.ReadAt(buf, int64(m.e.off)); err != nil {
			return fmt.Errorf("rewrite pack %d: %w", n, err)
		}
		num, off, err := p.write(buf)
		if err != nil {
			return fmt.Errorf("rewrite pack %d: %w", n, err)
		}
		p.mu.Lock()
		if cur, ok := p.index[m.id]; ok && cur.pack == n && cur.off == m.e.off {
			cur.pack, cur.off = num, off
			p.index[m.id] = cur
		}
		p.mu.Unlock()
	}
	return nil
}
