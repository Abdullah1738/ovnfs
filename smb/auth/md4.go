// Package auth implements NTLMv2 authentication and SMB signing primitives.
package auth

import (
	"encoding/binary"
	"math/bits"
)

// MD4 computes the RFC 1320 message digest used by NT password hashing.
func MD4(b []byte) [16]byte {
	n := uint64(len(b)) * 8
	var state = [4]uint32{0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476}
	for len(b) >= 64 {
		md4Block(&state, b[:64])
		b = b[64:]
	}
	var tail [128]byte
	copy(tail[:], b)
	tail[len(b)] = 0x80
	end := 64
	if len(b) >= 56 {
		end = 128
	}
	binary.LittleEndian.PutUint64(tail[end-8:], n)
	md4Block(&state, tail[:64])
	if end == 128 {
		md4Block(&state, tail[64:])
	}
	var out [16]byte
	for i, v := range state {
		binary.LittleEndian.PutUint32(out[i*4:], v)
	}
	return out
}

// md4Block folds one complete block into the digest state.
func md4Block(s *[4]uint32, b []byte) {
	var x [16]uint32
	for i := range x {
		x[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	a, c0, c, d := s[0], s[1], s[2], s[3]
	v := [4]uint32{a, c0, c, d}
	for i := range 48 {
		j := (-i) & 3
		p, q, r := v[(j+1)&3], v[(j+2)&3], v[(j+3)&3]
		var f uint32
		var k, shift int
		switch {
		case i < 16:
			f = (p & q) | (^p & r)
			k = i
			shift = [4]int{3, 7, 11, 19}[i&3]
		case i < 32:
			f = (p & q) | (p & r) | (q & r)
			k = ((i-16)%4)*4 + (i-16)/4
			shift = [4]int{3, 5, 9, 13}[i&3]
			f += 0x5a827999
		default:
			f = p ^ q ^ r
			k = [16]int{0, 8, 4, 12, 2, 10, 6, 14, 1, 9, 5, 13, 3, 11, 7, 15}[i-32]
			shift = [4]int{3, 9, 11, 15}[i&3]
			f += 0x6ed9eba1
		}
		v[j] = bits.RotateLeft32(v[j]+f+x[k], shift)
	}
	for i := range s {
		s[i] += v[i]
	}
}
