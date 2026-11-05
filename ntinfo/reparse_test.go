package ntinfo

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"testing"
)

func TestSymlinkReparse(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a/b/c", "a/b/c"},
		{"/x", "/x"},
		{`\??\C:\x`, "C:/x"},
		{`\??\UNC\srv\s`, "//srv/s"},
	} {
		b, err := SymlinkReparse(tc.in)
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if relative := binary.LittleEndian.Uint32(b[16:]) == 1; relative == (tc.in[0] == '/') {
			t.Errorf("%q: relative flag %v", tc.in, relative)
		}
		if got, err := ParseSymlinkReparse(b); err != nil || got != tc.want {
			t.Errorf("%q: got %q, %v; want %q", tc.in, got, err, tc.want)
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
