package fsys

import (
	"slices"
	"testing"
)

func TestWTF8RoundTrip(t *testing.T) {
	names := [][]uint16{
		{'a', 'b'},
		{0xE9},             // é
		{0xD83D, 0xDE00},   // surrogate pair
		{'x', 0xD800, 'y'}, // lone high surrogate
		{0xDC00},           // lone low surrogate
		{0xDC00, 0xD800},   // reversed pair stays two lone surrogates
		{0xD7FF, 0xE000, 0xFFFF},
	}
	for _, name := range names {
		wtf8 := appendWTF8(nil, name)
		if back := appendUTF16(nil, string(wtf8)); !slices.Equal(back, name) {
			t.Errorf("%x → %q → %x", name, wtf8, back)
		}
	}
	if got := string(appendWTF8(nil, []uint16{0xD83D, 0xDE00})); got != "😀" {
		t.Errorf("pair encodes as %q", got)
	}
}
