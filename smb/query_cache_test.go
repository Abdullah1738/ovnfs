package smb

import (
	"encoding/binary"
	"testing"
)

// TestSMBQueryInfoFollowsOtherHandles checks that QUERY_INFO, which reuses the
// metadata CREATE read, still reports a write, truncation and rename made
// through another client's handle right after.
func TestSMBQueryInfoFollowsOtherHandles(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, upper, "f", "abc") })
	reader, writer := guestClient(t, fixture), guestClient(t, fixture)
	held := mustOpen(t, reader, "f", 1, 0x40)
	size := func() uint64 {
		t.Helper()
		data, r := reader.queryInfo(held, 5) // FileStandardInformation
		reader.expectStatus(r, wireSuccess)
		if len(data) < 16 {
			t.Fatalf("short FileStandardInformation %x", data)
		}
		return binary.LittleEndian.Uint64(data[8:])
	}
	if got := size(); got != 3 {
		t.Fatalf("size after CREATE = %d, want 3", got)
	}
	other := mustOpen(t, writer, "f", 1, 0x40)
	writer.expectStatus(writer.write(other, 0, make([]byte, 100)), wireSuccess)
	if got := size(); got != 100 {
		t.Fatalf("size after another client's WRITE = %d, want 100", got)
	}
	writer.expectStatus(writer.setInfo(other, 20, binary.LittleEndian.AppendUint64(nil, 7)), wireSuccess) // FileEndOfFileInformation
	if got := size(); got != 7 {
		t.Fatalf("size after another client's truncation = %d, want 7", got)
	}
	writer.expectStatus(writer.rename(other, "g", false), wireSuccess)
	data, r := reader.queryInfo(held, 9) // FileNameInformation
	reader.expectStatus(r, wireSuccess)
	if name := utf16Decode(data[4 : 4+binary.LittleEndian.Uint32(data)]); name != `\g` {
		t.Fatalf("name after another client's rename = %q, want %q", name, `\g`)
	}
}
