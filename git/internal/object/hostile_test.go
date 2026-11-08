package object

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// packBuilder assembles pack streams entry by entry, for packs git would
// never produce.
type packBuilder struct {
	f     Format
	buf   bytes.Buffer
	count uint32
	zw    *zlib.Writer
}

func newPackBuilder(f Format) *packBuilder {
	b := &packBuilder{f: f}
	b.buf.WriteString("PACK\x00\x00\x00\x02\x00\x00\x00\x00")
	b.zw = zlib.NewWriter(nil)
	return b
}

// add appends an entry with the given header bytes and returns its offset.
func (b *packBuilder) add(header, payload []byte) int64 {
	off := int64(b.buf.Len())
	b.buf.Write(header)
	b.zw.Reset(&b.buf)
	b.zw.Write(payload)
	b.zw.Close()
	b.count++
	return off
}

func (b *packBuilder) whole(kind Kind, data []byte) int64 {
	return b.add(appendEntryHeader(nil, byte(kind), int64(len(data))), data)
}

func (b *packBuilder) ofsDelta(base int64, delta []byte) int64 {
	off := int64(b.buf.Len())
	hdr := appendEntryHeader(nil, typeOfsDelta, int64(len(delta)))
	return b.add(append(hdr, encodeOfs(uint64(off-base))...), delta)
}

func (b *packBuilder) bytes() []byte {
	p := bytes.Clone(b.buf.Bytes())
	binary.BigEndian.PutUint32(p[8:], b.count)
	h := b.f.New()
	h.Write(p)
	return h.Sum(p)
}

// encodeOfs writes an ofs-delta base distance in the pack offset encoding.
func encodeOfs(rel uint64) []byte {
	b := []byte{byte(rel & 0x7f)}
	for rel >>= 7; rel > 0; rel >>= 7 {
		rel--
		b = append([]byte{0x80 | byte(rel&0x7f)}, b...)
	}
	return b
}

func appendSize(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// insertDelta is a delta that ignores a base of baseLen bytes and inserts content.
func insertDelta(baseLen int, content []byte) []byte {
	d := appendSize(appendSize(nil, uint64(baseLen)), uint64(len(content)))
	for len(content) > 0 {
		n := min(len(content), 127)
		d = append(append(d, byte(n)), content[:n]...)
		content = content[n:]
	}
	return d
}

// chainPack is a pack of one blob and n ofs-deltas, each based on the last.
func chainPack(n int) ([]byte, []byte) {
	b := newPackBuilder(SHA1)
	prev := []byte("base")
	off := b.whole(Blob, prev)
	for i := 1; i <= n; i++ {
		next := fmt.Appendf(nil, "v%d", i)
		off = b.ofsDelta(off, insertDelta(len(prev), next))
		prev = next
	}
	return b.bytes(), prev
}

func TestIndexPackDeltaChainLimit(t *testing.T) {
	objects := t.TempDir()
	packDir := filepath.Join(objects, "pack")
	pack, tip := chainPack(maxDeltaChain)
	if _, n, err := IndexPack(bytes.NewReader(pack), packDir, SHA1, IndexPackOptions{}); err != nil || n != maxDeltaChain+1 {
		t.Fatalf("IndexPack of a %d-delta chain = %d, %v", maxDeltaChain, n, err)
	}
	db, err := Open(objects, SHA1)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id := Hash(SHA1, Blob, tip)
	if kind, data, err := db.Read(id); err != nil || kind != Blob || !bytes.Equal(data, tip) {
		t.Errorf("Read(tip) = %v %q %v", kind, data, err)
	}
	if kind, size, err := db.Header(id); err != nil || kind != Blob || size != int64(len(tip)) {
		t.Errorf("Header(tip) = %v %d %v", kind, size, err)
	}

	tooDeep, _ := chainPack(maxDeltaChain + 1)
	dir := t.TempDir()
	if _, _, err := IndexPack(bytes.NewReader(tooDeep), dir, SHA1, IndexPackOptions{}); err == nil {
		t.Error("pack with a chain readers reject was accepted")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("files left after rejection: %v", ents)
	}
}

func TestIndexPackMaxObjectSize(t *testing.T) {
	const limit = 1 << 20
	big := bytes.Repeat([]byte("0123456789abcdef"), 2*limit/16)
	base := bytes.Repeat([]byte{'b'}, 1<<16)
	// 32 copies of a 64 KiB base: a 2 MiB result from a few hundred bytes.
	expand := appendSize(appendSize(nil, uint64(len(base))), 32<<16)
	for range 32 {
		expand = append(expand, 0x80) // copy offset 0, size 0 = 0x10000
	}
	for name, build := range map[string]func(b *packBuilder){
		"declared object size": func(b *packBuilder) { b.add(appendEntryHeader(nil, byte(Blob), 1<<30), []byte("abc")) },
		"real object size":     func(b *packBuilder) { b.whole(Blob, big) },
		"declared delta result": func(b *packBuilder) {
			// A 1 GiB result declared over a single 1-byte insert.
			d := append(appendSize(appendSize(nil, 1), 1<<30), 1, 'y')
			b.ofsDelta(b.whole(Blob, []byte("x")), d)
		},
		"delta expansion": func(b *packBuilder) { b.ofsDelta(b.whole(Blob, base), expand) },
	} {
		t.Run(name, func(t *testing.T) {
			b := newPackBuilder(SHA1)
			build(b)
			pack := b.bytes()
			dir := t.TempDir()
			var err error
			n := allocatedBy(func() {
				_, _, err = IndexPack(bytes.NewReader(pack), dir, SHA1, IndexPackOptions{MaxObjectSize: limit})
			})
			if err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Errorf("IndexPack error %v, want the size limit", err)
			}
			if n > uint64(len(pack))+8<<20 {
				t.Errorf("IndexPack allocated %d bytes", n)
			}
			if ents, _ := os.ReadDir(dir); len(ents) != 0 {
				t.Errorf("files left after rejection: %v", ents)
			}
		})
	}
	// Within the limit the same objects are accepted.
	b := newPackBuilder(SHA1)
	b.whole(Blob, big)
	b.ofsDelta(b.whole(Blob, base), expand)
	if _, n, err := IndexPack(bytes.NewReader(b.bytes()), t.TempDir(), SHA1, IndexPackOptions{MaxObjectSize: 4 * limit}); err != nil || n != 3 {
		t.Errorf("IndexPack within the limit = %d, %v", n, err)
	}
}

func TestIndexPackHugeHeaderCount(t *testing.T) {
	pack := []byte("PACK\x00\x00\x00\x02\xff\xff\xff\xff")
	var err error
	if n := allocatedBy(func() { _, _, err = IndexPack(bytes.NewReader(pack), t.TempDir(), SHA1, IndexPackOptions{}) }); n > 16<<20 {
		t.Errorf("IndexPack allocated %d bytes for a header claiming 4G objects", n)
	}
	if err == nil {
		t.Error("truncated pack accepted")
	}
}

// allocatedBy returns the bytes fn allocates.
func allocatedBy(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestApplyDeltaDeclaredSizeBomb(t *testing.T) {
	base := make([]byte, 1<<20)
	ops := make([]byte, 1<<20) // reserved zero opcodes
	delta := append(appendSize(appendSize(nil, uint64(len(base))), 1<<40), ops...)
	var err error
	if n := allocatedBy(func() { _, err = applyDelta(base, delta) }); n > 1<<20 {
		t.Errorf("applyDelta allocated %d bytes for an invalid delta", n)
	}
	if err == nil {
		t.Error("invalid delta accepted")
	}
}

func TestInflateDeclaredSizeBomb(t *testing.T) {
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write([]byte("abc"))
	zw.Close()
	var err error
	if n := allocatedBy(func() { _, err = inflate(bytes.NewReader(z.Bytes()), 1<<30) }); n > 2*maxInitialAlloc {
		t.Errorf("inflate allocated %d bytes for a 3-byte stream", n)
	}
	if err == nil {
		t.Error("short stream accepted")
	}

	// A loose object whose header claims 1 GiB over 1 MiB of incompressible
	// content, plausible by compressed size alone.
	objects := t.TempDir()
	content := make([]byte, 1<<20)
	rand.NewChaCha8([32]byte{3, 4}).Read(content)
	z.Reset()
	zw, _ = zlib.NewWriterLevel(&z, zlib.NoCompression)
	zw.Write(append([]byte("blob 1073741824\x00"), content...))
	zw.Close()
	id := Hash(SHA1, Blob, content)
	path := loosePath(objects, id)
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, z.Bytes(), 0o444)
	db, err := Open(objects, SHA1)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n := allocatedBy(func() { _, _, err = db.Read(id) }); n > 2*maxInitialAlloc {
		t.Errorf("Read allocated %d bytes for a 1 MiB loose object", n)
	}
	if err == nil {
		t.Error("loose object shorter than its header accepted")
	}
}
