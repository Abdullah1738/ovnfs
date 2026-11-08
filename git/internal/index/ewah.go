package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"math/bits"
)

// bitmap is a decoded EWAH bitmap (technical/bitmap-format, appendix A), as
// used by the link, UNTR and FSMN extensions.
type bitmap struct {
	n     uint32 // number of bits
	words []uint64
}

var errEWAH = errors.New("malformed EWAH bitmap")

// decodeEWAH decodes the bitmap at the start of b and returns the bytes after
// it. maxBits is the number of items (entries, directories) the bitmap
// describes; a bitmap declaring more bits is malformed.
func decodeEWAH(b []byte, maxBits int) (bitmap, []byte, error) {
	if len(b) < 8 {
		return bitmap{}, nil, errEWAH
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(maxBits) {
		return bitmap{}, nil, fmt.Errorf("%w: %d bits for %d items", errEWAH, n, maxBits)
	}
	count := binary.BigEndian.Uint32(b[4:])
	b = b[8:]
	if uint64(len(b)) < uint64(count)*8+4 {
		return bitmap{}, nil, errEWAH
	}
	compressed, rest := b[:count*8], b[count*8+4:] // the trailing word is the last RLW's position, only needed for appending
	// Runs are counted in words; never expand beyond what n bits need, so a
	// corrupt run length cannot force a huge allocation.
	limit := (int(n) + 63) / 64
	bm := bitmap{n: n}
	for len(compressed) > 0 {
		rlw := binary.BigEndian.Uint64(compressed)
		compressed = compressed[8:]
		run := (rlw >> 1) & 0xffffffff
		literals := rlw >> 33
		fill := uint64(0)
		if rlw&1 != 0 {
			fill = ^uint64(0)
		}
		for ; run > 0 && len(bm.words) < limit; run-- {
			bm.words = append(bm.words, fill)
		}
		if uint64(len(compressed)) < literals*8 {
			return bitmap{}, nil, errEWAH
		}
		for range literals {
			if len(bm.words) < limit {
				bm.words = append(bm.words, binary.BigEndian.Uint64(compressed))
			}
			compressed = compressed[8:]
		}
	}
	return bm, rest, nil
}

// has reports bit i.
func (bm bitmap) has(i int) bool {
	if i < 0 || i >= int(bm.n) || i/64 >= len(bm.words) {
		return false
	}
	return bm.words[i/64]&(1<<(i%64)) != 0
}

// ones yields the positions of set bits in increasing order. Within a word,
// lower-order bits come first.
func (bm bitmap) ones() iter.Seq[int] {
	return func(yield func(int) bool) {
		for w, word := range bm.words {
			for word != 0 {
				i := w*64 + bits.TrailingZeros64(word)
				if i >= int(bm.n) || !yield(i) {
					return
				}
				word &= word - 1
			}
		}
	}
}
