package refs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// Reftable file layout constants (technical/reftable).
const (
	reftableMagic   = "REFT"
	headerLenV1     = 24
	headerLenV2     = 28
	footerExtra     = 44 // footer = header + five uint64 + CRC-32
	blockTypeRef    = 'r'
	blockTypeIndex  = 'i'
	blockTypeObj    = 'o'
	blockTypeLog    = 'g'
	refValDeletion  = 0
	refValOne       = 1
	refValTwo       = 2
	refValSymref    = 3
	logTypeDeletion = 0
	logTypeUpdate   = 1
	maxBlockLen     = 1<<24 - 1
	// maxIndexLevels bounds multi-level index descent so a corrupt index
	// pointing at itself cannot loop forever.
	maxIndexLevels = 16
)

var errCorruptTable = errors.New("corrupt reftable")

// refRecord is one ref_record: a value, a deletion tombstone, or a symref.
type refRecord struct {
	name        string
	updateIndex uint64
	valueType   byte
	value       object.ID
	peeled      object.ID
	target      string
}

// ref converts a non-deletion record to a Ref.
func (r refRecord) ref() Ref {
	return Ref{Name: r.name, ID: r.value, Peeled: r.peeled, Target: r.target}
}

// logRecord is one log_record; deleted marks a log_type 0 tombstone.
type logRecord struct {
	name        string
	updateIndex uint64
	deleted     bool
	entry       ReflogEntry
}

// table is one parsed reftable file, held in memory.
type table struct {
	data          []byte
	headerLen     int
	minIndex      uint64
	maxIndex      uint64
	format        object.Format
	refIndexPos   int
	logPos        int
	logIndexPos   int
	refSectionEnd int // where ref blocks stop: the first index, obj or log section, or the footer
	logSectionEnd int
}

// parseTable validates a reftable's header and footer.
func parseTable(data []byte, format object.Format) (*table, error) {
	if len(data) < headerLenV1 || string(data[:4]) != reftableMagic {
		return nil, fmt.Errorf("%w: bad magic", errCorruptTable)
	}
	t := &table{data: data, format: format}
	switch version := data[4]; version {
	case 1:
		t.headerLen = headerLenV1
		if format != object.SHA1 {
			return nil, fmt.Errorf("%w: version 1 table in a %s repository", errCorruptTable, format)
		}
	case 2:
		t.headerLen = headerLenV2
		if len(data) < headerLenV2 {
			return nil, fmt.Errorf("%w: short header", errCorruptTable)
		}
		if id := string(data[24:28]); id != hashID(format) {
			return nil, fmt.Errorf("%w: hash %q in a %s repository", errCorruptTable, id, format)
		}
	default:
		return nil, fmt.Errorf("%w: unsupported version %d", errCorruptTable, version)
	}
	footerLen := t.headerLen + footerExtra
	if len(data) < t.headerLen+footerLen {
		return nil, fmt.Errorf("%w: short file", errCorruptTable)
	}
	footer := data[len(data)-footerLen:]
	if !bytes.Equal(footer[:t.headerLen], data[:t.headerLen]) {
		return nil, fmt.Errorf("%w: footer does not match header", errCorruptTable)
	}
	if crc32.ChecksumIEEE(footer[:footerLen-4]) != binary.BigEndian.Uint32(footer[footerLen-4:]) {
		return nil, fmt.Errorf("%w: footer checksum mismatch", errCorruptTable)
	}
	t.minIndex = binary.BigEndian.Uint64(data[8:16])
	t.maxIndex = binary.BigEndian.Uint64(data[16:24])
	f := footer[t.headerLen:]
	end := uint64(len(data) - footerLen)
	fields := [5]uint64{}
	for i := range fields {
		fields[i] = binary.BigEndian.Uint64(f[8*i:])
		if i == 1 {
			fields[i] >>= 5 // obj_position << 5 | obj_id_len
		}
		if fields[i] > end {
			return nil, fmt.Errorf("%w: section offset out of range", errCorruptTable)
		}
	}
	t.refIndexPos, t.logPos, t.logIndexPos = int(fields[0]), int(fields[3]), int(fields[4])
	t.refSectionEnd = int(end)
	if t.logPos == 0 && t.headerLen < int(end) && data[t.headerLen] == blockTypeLog {
		// A log-only table: git writes log_position 0 even though its log
		// starts right after the header.
		t.logPos = t.headerLen
	}
	for _, pos := range []int{int(fields[0]), int(fields[1]), int(fields[2]), t.logPos} {
		if pos != 0 {
			t.refSectionEnd = min(t.refSectionEnd, pos)
		}
	}
	t.logSectionEnd = int(end)
	if t.logIndexPos != 0 {
		t.logSectionEnd = t.logIndexPos
	}
	return t, nil
}

// hashID is the version 2 header's hash_id for format.
func hashID(format object.Format) string {
	if format == object.SHA256 {
		return "s256"
	}
	return "sha1"
}

// getVarint decodes a varint in pack ofs-delta encoding.
func getVarint(b []byte) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, errCorruptTable
	}
	v := uint64(b[0] & 0x7f)
	n := 1
	for b[n-1]&0x80 != 0 {
		if n >= len(b) || n >= 10 {
			return 0, 0, errCorruptTable
		}
		v = (v+1)<<7 | uint64(b[n]&0x7f)
		n++
	}
	return v, n, nil
}

// appendVarint encodes v in pack ofs-delta encoding.
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

func uint24(b []byte) int { return int(b[0])<<16 | int(b[1])<<8 | int(b[2]) }

// block is one decoded ref, index or log block. Offsets (restarts, record
// positions) are relative to base, the position the block's offsets count
// from: the file start for the first block, else the block header.
type block struct {
	typ      byte
	buf      []byte // bytes from base to the end of the restart table
	recStart int    // first record
	recEnd   int    // start of the restart table
	restarts []int
	next     int // file position after the block (before any padding)
}

// readBlock decodes the block whose header is at file position pos.
func (t *table) readBlock(pos int) (*block, error) {
	if pos < 0 || pos+4 > len(t.data) {
		return nil, errCorruptTable
	}
	typ := t.data[pos]
	blockLen := uint24(t.data[pos+1 : pos+4])
	base := pos
	if pos == t.headerLen {
		base = 0 // the first block's length and offsets include the file header
	}
	b := &block{typ: typ, recStart: pos + 4 - base}
	switch typ {
	case blockTypeRef, blockTypeIndex, blockTypeObj:
		if base+blockLen > len(t.data) || blockLen < b.recStart+2 {
			return nil, errCorruptTable
		}
		b.buf = t.data[base : base+blockLen]
		b.next = base + blockLen
	case blockTypeLog:
		r := bytes.NewReader(t.data[pos+4:])
		zr, err := zlib.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("%w: log block: %v", errCorruptTable, err)
		}
		inflated, err := io.ReadAll(io.LimitReader(zr, maxBlockLen))
		if err != nil {
			return nil, fmt.Errorf("%w: log block: %v", errCorruptTable, err)
		}
		// bytes.Reader is an io.ByteReader, so the inflater consumed exactly
		// the compressed stream: what remains starts the next block.
		b.next = len(t.data) - r.Len()
		// Prefix the 4-byte header so offsets match the documented layout;
		// records are scanned linearly, so the restart offsets' base (which
		// for a log-only file's first block is not pinned down by the
		// documentation) never matters.
		b.buf = append(append(make([]byte, 0, 4+len(inflated)), t.data[pos:pos+4]...), inflated...)
		b.recStart = 4
		if len(b.buf) < b.recStart+2 {
			return nil, errCorruptTable
		}
	default:
		return nil, fmt.Errorf("%w: unknown block type %q", errCorruptTable, typ)
	}
	count := int(binary.BigEndian.Uint16(b.buf[len(b.buf)-2:]))
	b.recEnd = len(b.buf) - 2 - 3*count
	if count == 0 || b.recEnd < b.recStart {
		return nil, errCorruptTable
	}
	b.restarts = make([]int, count)
	for i := range count {
		b.restarts[i] = uint24(b.buf[b.recEnd+3*i:])
		if b.restarts[i] < b.recStart || b.restarts[i] >= b.recEnd {
			return nil, errCorruptTable
		}
	}
	return b, nil
}

// nextBlockPos skips NUL alignment padding after a block.
func (t *table) nextBlockPos(pos, limit int) int {
	for pos < limit && t.data[pos] == 0 {
		pos++
	}
	return pos
}

// recordKey decodes a record's prefix-compressed key at off, returning the
// key, the 3-bit value type and the offset of the value.
func (b *block) recordKey(off int, prev []byte) (key []byte, valueType byte, next int, err error) {
	prefix, n, err := getVarint(b.buf[off:b.recEnd])
	if err != nil {
		return nil, 0, 0, err
	}
	off += n
	sv, n, err := getVarint(b.buf[off:b.recEnd])
	if err != nil {
		return nil, 0, 0, err
	}
	off += n
	if prefix > uint64(len(prev)) || !fits(sv>>3, b.recEnd-off) {
		return nil, 0, 0, errCorruptTable
	}
	suffix := int(sv >> 3)
	key = append(append(make([]byte, 0, int(prefix)+suffix), prev[:prefix]...), b.buf[off:off+suffix]...)
	return key, byte(sv & 7), off + suffix, nil
}

// decodeRef decodes the ref_record at off whose predecessor's name is prev.
func (t *table) decodeRef(b *block, off int, prev []byte) (refRecord, []byte, int, error) {
	key, vt, off, err := b.recordKey(off, prev)
	if err != nil {
		return refRecord{}, nil, 0, err
	}
	delta, n, err := getVarint(b.buf[off:b.recEnd])
	if err != nil {
		return refRecord{}, nil, 0, err
	}
	off += n
	rec := refRecord{name: string(key), updateIndex: t.minIndex + delta, valueType: vt}
	size := t.format.Size()
	readID := func() (object.ID, error) {
		if off+size > b.recEnd {
			return object.ID{}, errCorruptTable
		}
		id, err := object.IDFromRaw(t.format, b.buf[off:off+size])
		off += size
		return id, err
	}
	switch vt {
	case refValDeletion:
	case refValOne, refValTwo:
		if rec.value, err = readID(); err != nil {
			return refRecord{}, nil, 0, err
		}
		if vt == refValTwo {
			if rec.peeled, err = readID(); err != nil {
				return refRecord{}, nil, 0, err
			}
		}
	case refValSymref:
		l, n, err := getVarint(b.buf[off:b.recEnd])
		if err != nil || !fits(l, b.recEnd-off-n) {
			return refRecord{}, nil, 0, errCorruptTable
		}
		rec.target = string(b.buf[off+n : off+n+int(l)])
		off += n + int(l)
	default:
		return refRecord{}, nil, 0, fmt.Errorf("%w: reserved ref value type %d", errCorruptTable, vt)
	}
	return rec, key, off, nil
}

// decodeIndex decodes an index_record: the last key of a block and the
// block's position.
func (b *block) decodeIndex(off int, prev []byte) (key []byte, pos int, next int, err error) {
	key, _, off, err = b.recordKey(off, prev)
	if err != nil {
		return nil, 0, 0, err
	}
	p, n, err := getVarint(b.buf[off:b.recEnd])
	if err != nil {
		return nil, 0, 0, err
	}
	if !fits(p, math.MaxInt) { // readBlock rejects positions outside the file
		return nil, 0, 0, errCorruptTable
	}
	return key, int(p), off + n, nil
}

// fits reports whether a length n decoded from a table fits in the avail
// bytes left. Comparing unsigned before converting keeps a huge varint from
// turning into a negative int that slips past an addition-based check.
func fits(n uint64, avail int) bool { return avail >= 0 && n <= uint64(avail) }

// seekRestart returns the offset of the last restart point whose key is
// <= key, or the first restart point if every key is greater.
func (b *block) seekRestart(key []byte) (int, error) {
	lo, hi := 0, len(b.restarts) // invariant: restarts[:lo] <= key < restarts[hi:]
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		k, _, _, err := b.recordKey(b.restarts[mid], nil)
		if err != nil {
			return 0, err
		}
		if bytes.Compare(k, key) <= 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return b.restarts[max(lo-1, 0)], nil
}

// lookupRef finds the record for name: through the ref index when the table
// has one, else by scanning ref blocks, binary searching each block's
// restart points.
func (t *table) lookupRef(name string) (refRecord, bool, error) {
	key := []byte(name)
	if t.refIndexPos != 0 {
		pos := t.refIndexPos
		for range maxIndexLevels {
			b, err := t.readBlock(pos)
			if err != nil {
				return refRecord{}, false, err
			}
			switch b.typ {
			case blockTypeRef:
				rec, found, _, err := t.seekInRefBlock(b, key)
				return rec, found, err
			case blockTypeIndex:
				next, ok, err := b.seekIndex(key)
				if err != nil || !ok {
					return refRecord{}, false, err
				}
				// The first block starts at the file start, before the
				// header; either spelling of its position finds it.
				pos = max(next, t.headerLen)
			default:
				return refRecord{}, false, fmt.Errorf("%w: ref index points at block %q", errCorruptTable, b.typ)
			}
		}
		return refRecord{}, false, fmt.Errorf("%w: ref index too deep", errCorruptTable)
	}
	for pos := t.headerLen; pos < t.refSectionEnd; {
		b, err := t.readBlock(pos)
		if err != nil {
			return refRecord{}, false, err
		}
		if b.typ != blockTypeRef {
			break
		}
		rec, found, passed, err := t.seekInRefBlock(b, key)
		if err != nil || found || passed {
			return rec, found, err
		}
		pos = t.nextBlockPos(b.next, t.refSectionEnd)
	}
	return refRecord{}, false, nil
}

// seekIndex returns the position of the block that would hold key: the
// first index record whose key is >= key.
func (b *block) seekIndex(key []byte) (int, bool, error) {
	off, err := b.seekRestart(key)
	if err != nil {
		return 0, false, err
	}
	var prev []byte
	for off < b.recEnd {
		k, pos, next, err := b.decodeIndex(off, prev)
		if err != nil {
			return 0, false, err
		}
		if bytes.Compare(k, key) >= 0 {
			return pos, true, nil
		}
		prev, off = k, next
	}
	return 0, false, nil
}

// seekInRefBlock searches one ref block; passed reports that the block holds
// a key greater than key, so later blocks cannot hold it.
func (t *table) seekInRefBlock(b *block, key []byte) (rec refRecord, found, passed bool, err error) {
	off, err := b.seekRestart(key)
	if err != nil {
		return refRecord{}, false, false, err
	}
	var prev []byte
	for off < b.recEnd {
		r, k, next, err := t.decodeRef(b, off, prev)
		if err != nil {
			return refRecord{}, false, false, err
		}
		switch c := bytes.Compare(k, key); {
		case c == 0:
			return r, true, false, nil
		case c > 0:
			return refRecord{}, false, true, nil
		}
		prev, off = k, next
	}
	return refRecord{}, false, false, nil
}

// refs returns every ref record in the table in name order, deletions
// included.
func (t *table) refs() ([]refRecord, error) {
	var out []refRecord
	for pos := t.headerLen; pos < t.refSectionEnd; {
		b, err := t.readBlock(pos)
		if err != nil {
			return nil, err
		}
		if b.typ != blockTypeRef {
			break
		}
		var prev []byte
		for off := b.recStart; off < b.recEnd; {
			rec, key, next, err := t.decodeRef(b, off, prev)
			if err != nil {
				return nil, err
			}
			out = append(out, rec)
			prev, off = key, next
		}
		pos = t.nextBlockPos(b.next, t.refSectionEnd)
	}
	return out, nil
}

// logs returns the table's log records for name (every name if all is set),
// in key order: by name, then newest first.
func (t *table) logs(name string, all bool) ([]logRecord, error) {
	if t.logPos == 0 {
		return nil, nil
	}
	var out []logRecord
	for pos := t.logPos; pos < t.logSectionEnd; {
		b, err := t.readBlock(pos)
		if err != nil {
			return nil, err
		}
		if b.typ != blockTypeLog {
			break
		}
		var prev []byte
		for off := b.recStart; off < b.recEnd; {
			rec, key, next, err := t.decodeLog(b, off, prev)
			if err != nil {
				return nil, err
			}
			if all || rec.name == name {
				out = append(out, rec)
			}
			prev, off = key, next
		}
		pos = b.next
	}
	return out, nil
}

// decodeLog decodes the log_record at off.
func (t *table) decodeLog(b *block, off int, prev []byte) (logRecord, []byte, int, error) {
	key, lt, off, err := b.recordKey(off, prev)
	if err != nil {
		return logRecord{}, nil, 0, err
	}
	if len(key) < 9 || key[len(key)-9] != 0 {
		return logRecord{}, nil, 0, fmt.Errorf("%w: bad log key", errCorruptTable)
	}
	rec := logRecord{
		name:        string(key[:len(key)-9]),
		updateIndex: ^binary.BigEndian.Uint64(key[len(key)-8:]),
	}
	switch lt {
	case logTypeDeletion:
		rec.deleted = true
		return rec, key, off, nil
	case logTypeUpdate:
	default:
		return logRecord{}, nil, 0, fmt.Errorf("%w: unknown log type %d", errCorruptTable, lt)
	}
	d := decoder{buf: b.buf[:b.recEnd], off: off}
	size := t.format.Size()
	rec.entry.Old, _ = object.IDFromRaw(t.format, d.bytes(size))
	rec.entry.New, _ = object.IDFromRaw(t.format, d.bytes(size))
	rec.entry.Committer.Name = string(d.varBytes())
	rec.entry.Committer.Email = string(d.varBytes())
	seconds := d.varint()
	tz := int16(binary.BigEndian.Uint16(d.bytes(2)))
	// git terminates every stored message with a newline (an empty message
	// is "\n"); the files backend's lines carry none, so strip it.
	rec.entry.Message = strings.TrimSuffix(string(d.varBytes()), "\n")
	if d.err != nil {
		return logRecord{}, nil, 0, d.err
	}
	rec.entry.Committer.When = time.Unix(int64(seconds), 0).In(time.FixedZone("", tzOffsetSeconds(tz)))
	return rec, key, d.off, nil
}

// tzOffsetSeconds decodes a log record's tz_offset. The documentation calls
// it minutes from GMT, but git stores the signature's ±hhmm digits as a
// decimal number (+0130 as 130, -0845 as -845), and git is the format's
// reader that matters.
func tzOffsetSeconds(tz int16) int {
	v := int(tz)
	sign := 1
	if v < 0 {
		sign, v = -1, -v
	}
	return sign * (v/100*60 + v%100) * 60
}

// tzOffsetField encodes a UTC offset in seconds as git's tz_offset.
func tzOffsetField(offset int) int16 {
	sign := 1
	if offset < 0 {
		sign, offset = -1, -offset
	}
	minutes := offset / 60
	return int16(sign * (minutes/60*100 + minutes%60))
}

// decoder reads a sequence of fields, remembering the first error.
type decoder struct {
	buf []byte
	off int
	err error
}

var zeros [64]byte

// varBytes reads a varint length followed by that many bytes.
func (d *decoder) varBytes() []byte {
	n := d.varint()
	if d.err == nil && !fits(n, len(d.buf)-d.off) {
		d.err = errCorruptTable
	}
	if d.err != nil {
		return nil
	}
	return d.bytes(int(n))
}

func (d *decoder) bytes(n int) []byte {
	if d.err != nil || n < 0 || d.off+n > len(d.buf) {
		d.err = errCorruptTable
		if n >= 0 && n <= len(zeros) {
			return zeros[:n] // keeps callers' fixed-size parsing panic-free
		}
		return nil
	}
	b := d.buf[d.off : d.off+n]
	d.off += n
	return b
}

func (d *decoder) varint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n, err := getVarint(d.buf[d.off:])
	if err != nil {
		d.err = err
		return 0
	}
	d.off += n
	return v
}
