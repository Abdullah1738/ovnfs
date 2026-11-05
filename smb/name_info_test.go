package smb

import (
	"encoding/binary"
	"testing"
)

// TestSMBNameInformation pins the two share-relative name forms Windows
// clients rely on: GetFinalPathNameByHandle appends FileNormalizedNameInformation
// to \\server\share\, so a leading backslash there yields an invalid
// "\\?\UNC\server\share\\dir\file" path (Rust's fs::canonicalize, rustc --extern).
func TestSMBNameInformation(t *testing.T) {
	fixture := startSMB(t, nil, func(lower, upper string) { putDiskFile(t, upper, "dir/file.txt", "x") })
	c := guestClient(t, fixture)
	name := func(id wireID, class byte) string {
		t.Helper()
		data, r := c.queryInfo(id, class)
		c.expectStatus(r, wireSuccess)
		if len(data) < 4 || int(binary.LittleEndian.Uint32(data))+4 > len(data) {
			t.Fatalf("class %d: malformed name reply %x", class, data)
		}
		return utf16Decode(data[4 : 4+binary.LittleEndian.Uint32(data)])
	}
	file := mustOpen(t, c, "dir/file.txt", 1, 0x40)
	if got := name(file, 9); got != `\dir\file.txt` {
		t.Fatalf("FileNameInformation = %q, want %q", got, `\dir\file.txt`)
	}
	if got := name(file, 48); got != `dir\file.txt` {
		t.Fatalf("FileNormalizedNameInformation = %q, want %q", got, `dir\file.txt`)
	}
	root := mustOpen(t, c, "", 1, 1)
	if got := name(root, 48); got != "" {
		t.Fatalf("FileNormalizedNameInformation of the share root = %q, want empty", got)
	}
}
