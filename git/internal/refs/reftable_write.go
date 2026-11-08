package refs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"hash/crc32"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

const (
	// writeBlockSize is the block size git itself writes by default.
	writeBlockSize = 4096
	// restartInterval is how many records share one restart point.
	restartInterval = 16
)

// tableWriter builds one reftable file whose records all carry a single
// update index, as a ref transaction appended to the stack does.
type tableWriter struct {
	format      object.Format
	updateIndex uint64
}

// write encodes refs (sorted by name) and logs (sorted by name, then newest
// first) into a complete table. Ref blocks are aligned to writeBlockSize, so
// no ref index is needed; the log section is a single block.
func (w tableWriter) write(refs []refRecord, logs []logRecord) ([]byte, error) {
	header := w.header()
	out := bytes.Clone(header)
	if len(refs) > 0 {
		var err error
		if out, err = w.writeRefBlocks(header, refs); err != nil {
			return nil, err
		}
	}
	logPos := 0
	if len(logs) > 0 {
		logPos = len(out)
		var err error
		if out, err = w.writeLogBlock(out, logs); err != nil {
			return nil, err
		}
	}
	footerStart := len(out)
	out = append(out, header...)
	out = binary.BigEndian.AppendUint64(out, 0) // ref_index_position
	out = binary.BigEndian.AppendUint64(out, 0) // obj_position << 5 | obj_id_len
	out = binary.BigEndian.AppendUint64(out, 0) // obj_index_position
	out = binary.BigEndian.AppendUint64(out, uint64(logPos))
	out = binary.BigEndian.AppendUint64(out, 0) // log_index_position
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[footerStart:])), nil
}

func (w tableWriter) header() []byte {
	h := []byte(reftableMagic)
	version := byte(1) // recommended for SHA-1 for compatibility
	if w.format == object.SHA256 {
		version = 2
	}
	h = append(h, version)
	h = appendUint24(h, writeBlockSize)
	h = binary.BigEndian.AppendUint64(h, w.updateIndex)
	h = binary.BigEndian.AppendUint64(h, w.updateIndex)
	if version == 2 {
		h = append(h, hashID(w.format)...)
	}
	return h
}

// blockBuilder accumulates prefix-compressed records for one block.
type blockBuilder struct {
	buf      []byte // bytes offsets count from: the file header for the first block, then the block header and records
	hdr      int    // position of the 4-byte block header in buf
	restarts []int
	count    int
	prevKey  []byte
}

// newBlockBuilder starts a block of type typ; prefix is the file header
// when this is the file's first block, whose offsets and length include it.
func newBlockBuilder(typ byte, prefix []byte) *blockBuilder {
	b := &blockBuilder{hdr: len(prefix)}
	b.buf = append(append(b.buf, prefix...), typ, 0, 0, 0)
	return b
}

// encodeRecord returns the record bytes for key at this point of the block,
// and whether it starts a restart point.
func (b *blockBuilder) encodeRecord(key []byte, valueType byte, value []byte) ([]byte, bool) {
	restart := b.count%restartInterval == 0
	prefix := 0
	if !restart {
		for prefix < len(key) && prefix < len(b.prevKey) && key[prefix] == b.prevKey[prefix] {
			prefix++
		}
	}
	rec := appendVarint(nil, uint64(prefix))
	rec = appendVarint(rec, uint64(len(key)-prefix)<<3|uint64(valueType))
	rec = append(rec, key[prefix:]...)
	return append(rec, value...), restart
}

// size is the finished block length if rec (a restart point when restart)
// were added.
func (b *blockBuilder) size(rec []byte, restart bool) int {
	restarts := len(b.restarts)
	if restart {
		restarts++
	}
	return len(b.buf) + len(rec) + 3*restarts + 2
}

func (b *blockBuilder) add(key, rec []byte, restart bool) {
	if restart {
		b.restarts = append(b.restarts, len(b.buf))
	}
	b.buf = append(b.buf, rec...)
	b.prevKey = key
	b.count++
}

// finish appends the restart table and fills in block_len.
func (b *blockBuilder) finish() []byte {
	for _, r := range b.restarts {
		b.buf = appendUint24(b.buf, r)
	}
	b.buf = binary.BigEndian.AppendUint16(b.buf, uint16(len(b.restarts)))
	appendUint24(b.buf[b.hdr+1:b.hdr+1], len(b.buf))
	return b.buf
}

func appendUint24(b []byte, v int) []byte { return append(b, byte(v>>16), byte(v>>8), byte(v)) }

// writeRefBlocks returns the file header followed by ref blocks: the first
// block shares the file's first writeBlockSize bytes with the header.
func (w tableWriter) writeRefBlocks(header []byte, refs []refRecord) ([]byte, error) {
	b := newBlockBuilder(blockTypeRef, header)
	var out []byte
	for _, r := range refs {
		var value []byte
		value = appendVarint(value, r.updateIndex-w.updateIndex)
		switch r.valueType {
		case refValOne:
			value = append(value, r.value.Raw()...)
		case refValTwo:
			value = append(append(value, r.value.Raw()...), r.peeled.Raw()...)
		case refValSymref:
			value = appendVarint(value, uint64(len(r.target)))
			value = append(value, r.target...)
		case refValDeletion:
		default:
			return nil, fmt.Errorf("refs: unknown ref value type %d", r.valueType)
		}
		key := []byte(r.name)
		rec, restart := b.encodeRecord(key, r.valueType, value)
		if b.count > 0 && b.size(rec, restart) > writeBlockSize {
			out = padBlock(out, b)
			b = newBlockBuilder(blockTypeRef, nil)
			rec, restart = b.encodeRecord(key, r.valueType, value)
		}
		if b.size(rec, restart) > writeBlockSize {
			return nil, fmt.Errorf("refs: reference name %q too long for a reftable block", r.name)
		}
		b.add(key, rec, restart)
	}
	return append(out, b.finish()...), nil
}

// padBlock appends a finished, non-final block padded to the block size.
func padBlock(out []byte, b *blockBuilder) []byte {
	out = append(out, b.finish()...)
	for len(out)%writeBlockSize != 0 {
		out = append(out, 0)
	}
	return out
}

func (w tableWriter) writeLogBlock(out []byte, logs []logRecord) ([]byte, error) {
	b := newBlockBuilder(blockTypeLog, nil)
	for _, l := range logs {
		key := binary.BigEndian.AppendUint64(append([]byte(l.name), 0), ^l.updateIndex)
		var value []byte
		valueType := byte(logTypeDeletion)
		if !l.deleted {
			valueType = logTypeUpdate
			e := l.entry
			value = append(value, e.Old.Raw()...)
			value = append(value, e.New.Raw()...)
			value = appendVarint(value, uint64(len(e.Committer.Name)))
			value = append(value, e.Committer.Name...)
			value = appendVarint(value, uint64(len(e.Committer.Email)))
			value = append(value, e.Committer.Email...)
			value = appendVarint(value, uint64(e.Committer.When.Unix()))
			_, offset := e.Committer.When.Zone()
			value = binary.BigEndian.AppendUint16(value, uint16(tzOffsetField(offset)))
			// git stores messages newline-terminated.
			value = appendVarint(value, uint64(len(e.Message)+1))
			value = append(append(value, e.Message...), '\n')
		}
		rec, restart := b.encodeRecord(key, valueType, value)
		b.add(key, rec, restart)
	}
	block := b.finish()
	if len(block) > maxBlockLen {
		return nil, fmt.Errorf("refs: reflog records exceed one reftable log block")
	}
	out = append(out, block[:4]...)
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(block[4:])
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return append(out, z.Bytes()...), nil
}
