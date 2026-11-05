package smb

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestSMBNonsparseAllocationCoversEOF exercises a sparse host extent created by
// a real positioned WRITE, without advertising sparse-file semantics to SMB.
func TestSMBNonsparseAllocationCoversEOF(t *testing.T) {
	const name = "archive.rlib"
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, upper, name, "") })
	c := guestClient(t, fixture)
	writer := mustOpen(t, c, name, 1, 0x40)
	const offset = 48<<20 + 37
	payload := []byte("archive member bytes")
	c.expectStatus(c.write(writer, offset, payload), wireSuccess)
	fixture.flush(t)
	eof := uint64(offset + len(payload))
	native, err := os.Stat(filepath.Join(fixture.upper, name))
	if err != nil {
		t.Fatal(err)
	}
	if native.Size() != int64(eof) {
		t.Fatalf("native EOF %d, want %d", native.Size(), eof)
	}
	if physical, ok := statBlocks(native); !ok || physical >= eof {
		t.Skip("requires native sparse-file allocation accounting")
	}

	// READ must expose the unwritten hole as zero bytes and retain the payload.
	data, r := c.read(writer, offset-64, uint32(64+len(payload)))
	c.expectStatus(r, wireSuccess)
	want := append(make([]byte, 64), payload...)
	if !bytes.Equal(data, want) {
		t.Fatalf("hole/payload READ = %x, want %x", data, want)
	}
	data, r = c.read(writer, 0, 64)
	c.expectStatus(r, wireSuccess)
	if !bytes.Equal(data, make([]byte, 64)) {
		t.Fatalf("initial hole READ = %x, want zero bytes", data)
	}

	check := func(stage string, data []byte, allocationAt, eofAt, attributesAt int) {
		t.Helper()
		if len(data) < allocationAt+8 || len(data) < eofAt+8 || len(data) < attributesAt+4 {
			t.Fatalf("%s: short metadata reply", stage)
		}
		allocation := binary.LittleEndian.Uint64(data[allocationAt:])
		size := binary.LittleEndian.Uint64(data[eofAt:])
		attributes := binary.LittleEndian.Uint32(data[attributesAt:])
		if attributes&(0x200|0x800) != 0 {
			t.Fatalf("%s: unexpected sparse/compressed attributes %#x", stage, attributes)
		}
		if size != eof || allocation < eof || allocation%4096 != 0 {
			t.Fatalf("%s: EOF=%d allocation=%d, want EOF=%d and covering cluster-aligned allocation", stage, size, allocation, eof)
		}
	}

	body := wireCreateBody(name, 1, 0x40)
	binary.LittleEndian.PutUint32(body[24:], 0x80)
	r = c.exchange(5, body)
	c.expectStatus(r, wireSuccess)
	check("metadata CREATE", r.body, 40, 48, 56)
	if len(r.body) < 88 {
		t.Fatal("short metadata CREATE response")
	}
	var metadata wireID
	copy(metadata[:], r.body[64:80])
	info, r := c.queryInfo(metadata, 18)
	c.expectStatus(r, wireSuccess)
	check("QUERY_INFO", info, 40, 48, 32)
	info, r = c.queryInfo(metadata, 100)
	c.expectStatus(r, wireSuccess)
	check("POSIX QUERY_INFO", info, 40, 32, 48)
	r = c.exchange(6, wirePostqueryClose(metadata))
	c.expectStatus(r, wireSuccess)
	if len(r.body) < 60 || binary.LittleEndian.Uint16(r.body[2:])&1 == 0 {
		t.Fatal("CLOSE omitted postquery attributes")
	}
	check("metadata CLOSE", r.body, 40, 48, 56)

	dir := mustOpen(t, c, "", 1, 1)
	body = make([]byte, 32)
	binary.LittleEndian.PutUint16(body, 33)
	body[2], body[3] = 1, 1
	copy(body[8:], dir[:])
	pattern := wireUTF16(name)
	binary.LittleEndian.PutUint16(body[24:], 96)
	binary.LittleEndian.PutUint16(body[26:], uint16(len(pattern)))
	binary.LittleEndian.PutUint32(body[28:], 4096)
	r = c.exchange(14, append(body, pattern...))
	c.expectStatus(r, wireSuccess)
	if len(r.body) < 8 {
		t.Fatal("short QUERY_DIRECTORY response")
	}
	entry := c.wireSecurity(r, int(binary.LittleEndian.Uint16(r.body[2:])), int(binary.LittleEndian.Uint32(r.body[4:])))
	check("QUERY_DIRECTORY", entry, 48, 40, 56)
	c.expectStatus(c.close(dir), wireSuccess)
	r = c.exchange(6, wirePostqueryClose(writer))
	c.expectStatus(r, wireSuccess)
	check("writer CLOSE", r.body, 40, 48, 56)
}

// statBlocks reads native st_blocks allocation in bytes when the platform exposes it.
func statBlocks(info os.FileInfo) (uint64, bool) {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0, false
	}
	if f := v.FieldByName("Blocks"); f.IsValid() && f.CanInt() && f.Int() >= 0 {
		return uint64(f.Int()) * 512, true
	}
	return 0, false
}
