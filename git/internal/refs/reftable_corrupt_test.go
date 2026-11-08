package refs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// craftedTable assembles a SHA-1 table by hand: the file header, a first
// block of type typ holding records (one restart point at its first record),
// optionally a second block at a known position, and a footer.
type craftedTable struct {
	typ         byte
	records     []byte
	extra       func(pos int) []byte // a block appended after the first; nil for none
	refIndexPos bool                 // point the footer's ref index at the extra block
}

func (c craftedTable) bytes() []byte {
	header := tableWriter{format: object.SHA1, updateIndex: 1}.header()
	var out []byte
	switch c.typ {
	case blockTypeLog:
		body := append(bytes.Clone(c.records), 0, 0, 4, 0, 1) // restart at offset 4, count 1
		out = appendUint24(append(bytes.Clone(header), blockTypeLog), 4+len(body))
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		zw.Write(body)
		zw.Close()
		out = append(out, z.Bytes()...)
	default:
		first := len(header) + 4 // the first record's offset from the file start
		out = append(append(bytes.Clone(header), c.typ, 0, 0, 0), c.records...)
		out = appendUint24(out, first)
		out = binary.BigEndian.AppendUint16(out, 1)
		appendUint24(out[len(header)+1:len(header)+1], len(out))
	}
	extraPos := 0
	if c.extra != nil {
		extraPos = len(out)
		out = append(out, c.extra(extraPos)...)
	}
	footerStart := len(out)
	out = append(out, header...)
	refIndex := 0
	if c.refIndexPos {
		refIndex = extraPos
	}
	for _, field := range []int{refIndex, 0, 0, 0, 0} {
		out = binary.BigEndian.AppendUint64(out, uint64(field))
	}
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[footerStart:]))
}

// record encodes a record key with no prefix compression.
func record(key string, valueType byte, value ...[]byte) []byte {
	b := appendVarint(nil, 0)
	b = appendVarint(b, uint64(len(key))<<3|uint64(valueType))
	b = append(b, key...)
	for _, v := range value {
		b = append(b, v...)
	}
	return b
}

func varint(v uint64) []byte { return appendVarint(nil, v) }

// TestCorruptReftableLengths feeds the reader tables whose varint lengths and
// positions are absurd (up to 2^64-1, which would turn negative as an int)
// and checks every read path reports corruption instead of panicking.
func TestCorruptReftableLengths(t *testing.T) {
	id := make([]byte, object.SHA1.Size())
	logKey := "HEAD\x00" + string(binary.BigEndian.AppendUint64(nil, ^uint64(1)))
	logData := func(nameLen, emailLen, msgLen uint64) []byte {
		b := append(bytes.Clone(id), id...)
		b = append(append(b, varint(nameLen)...), 'n')
		b = append(append(b, varint(emailLen)...), 'e')
		b = append(append(b, varint(0)...), 0, 0)
		return append(append(b, varint(msgLen)...), 'm')
	}
	validRef := record("refs/a", refValOne, varint(0), id)
	indexBlock := func(position uint64) func(int) []byte {
		return func(int) []byte {
			b := append([]byte{blockTypeIndex, 0, 0, 0}, record("refs/z", 0, varint(position))...)
			b = appendUint24(b, 4)
			b = binary.BigEndian.AppendUint16(b, 1)
			appendUint24(b[1:1], len(b))
			return b
		}
	}
	cases := map[string]craftedTable{
		"symref target length 2^64-1": {typ: blockTypeRef, records: record("HEAD", refValSymref, varint(0), varint(math.MaxUint64), []byte("refs/heads/x"))},
		"symref target length 2^63":   {typ: blockTypeRef, records: record("HEAD", refValSymref, varint(0), varint(1<<63), []byte("x"))},
		"key suffix length 2^61-1": {typ: blockTypeRef, records: append(append(varint(0), varint(math.MaxUint64&^7|refValOne)...),
			[]byte("refs/heads/x")...)},
		"key prefix length 2^64-1":  {typ: blockTypeRef, records: append(varint(math.MaxUint64), varint(1<<3|refValOne)...)},
		"log name length 2^64-1":    {typ: blockTypeLog, records: record(logKey, logTypeUpdate, logData(math.MaxUint64, 1, 1))},
		"log email length 2^63":     {typ: blockTypeLog, records: record(logKey, logTypeUpdate, logData(1, 1<<63, 1))},
		"log message length 2^64-1": {typ: blockTypeLog, records: record(logKey, logTypeUpdate, logData(1, 1, math.MaxUint64))},
		"index position 2^64-1":     {typ: blockTypeRef, records: validRef, extra: indexBlock(math.MaxUint64), refIndexPos: true},
		"index position 2^63":       {typ: blockTypeRef, records: validRef, extra: indexBlock(1 << 63), refIndexPos: true},
		"index position past end":   {typ: blockTypeRef, records: validRef, extra: indexBlock(1 << 40), refIndexPos: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			tb, err := parseTable(c.bytes(), object.SHA1)
			if err != nil {
				t.Fatalf("parseTable rejected the crafted framing: %v", err)
			}
			_, _, lookupErr := tb.lookupRef("refs/b")
			_, _, headErr := tb.lookupRef("HEAD")
			_, refsErr := tb.refs()
			_, logsErr := tb.logs("", true)
			corrupt := false
			for _, err := range []error{lookupErr, headErr, refsErr, logsErr} {
				if err != nil && !errors.Is(err, errCorruptTable) {
					t.Errorf("error %v does not wrap errCorruptTable", err)
				}
				corrupt = corrupt || err != nil
			}
			if !corrupt {
				t.Error("no read path reported corruption")
			}
		})
	}
}
