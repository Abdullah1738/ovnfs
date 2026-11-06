package ntinfo

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"testing"
	"unicode/utf16"
)

// wireName decodes a UTF-16LE reparse name.
func wireName(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

func TestSymlinkReparse(t *testing.T) {
	for _, tc := range []struct {
		target, substitute, print string
		relative                  bool
	}{
		{"a/b/c", `a\b\c`, `a\b\c`, true},
		{"../x", `..\x`, `..\x`, true},
		{"/x", `\x`, `\x`, false},
		{"C:/x/y", `\??\C:\x\y`, `C:\x\y`, false},
		{"//srv/s/x", `\??\UNC\srv\s\x`, `\\srv\s\x`, false},
	} {
		b, err := SymlinkReparse(tc.target)
		if err != nil {
			t.Fatalf("%q: %v", tc.target, err)
		}
		name := func(off, n int) string { return wireName(b[20+off : 20+off+n]) }
		if got := name(int(binary.LittleEndian.Uint16(b[8:])), int(binary.LittleEndian.Uint16(b[10:]))); got != tc.substitute {
			t.Errorf("%q: substitute name %q, want %q", tc.target, got, tc.substitute)
		}
		if got := name(int(binary.LittleEndian.Uint16(b[12:])), int(binary.LittleEndian.Uint16(b[14:]))); got != tc.print {
			t.Errorf("%q: print name %q, want %q", tc.target, got, tc.print)
		}
		if relative := binary.LittleEndian.Uint32(b[16:]) == 1; relative != tc.relative {
			t.Errorf("%q: relative flag %v", tc.target, relative)
		}
		if got, err := ParseSymlinkReparse(b); err != nil || got != tc.target {
			t.Errorf("%q: parsed %q, %v", tc.target, got, err)
		}
	}
	b, _ := SymlinkReparse("x")
	if _, err := ParseSymlinkReparse(b[:len(b)-2]); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("truncated: %v", err)
	}
	binary.LittleEndian.PutUint32(b, 0x80000014)
	if _, err := ParseSymlinkReparse(b); !errors.Is(err, ErrNotSymlink) {
		t.Errorf("other tag: %v", err)
	}
}
