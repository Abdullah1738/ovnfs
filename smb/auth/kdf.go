package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// KDF derives bits key bits using SP800-108 counter-mode HMAC-SHA256.
func KDF(key, label, context []byte, bits int) []byte {
	if bits <= 0 {
		return nil
	}
	out := make([]byte, 0, (bits+7)/8)
	var counter, length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(bits))
	for i := uint32(1); len(out) < (bits+7)/8; i++ {
		binary.BigEndian.PutUint32(counter[:], i)
		h := hmac.New(sha256.New, key)
		h.Write(counter[:])
		h.Write(label)
		h.Write([]byte{0})
		h.Write(context)
		h.Write(length[:])
		out = append(out, h.Sum(nil)...)
	}
	out = out[:(bits+7)/8]
	if bits%8 != 0 {
		out[len(out)-1] &= byte(0xff << (8 - bits%8))
	}
	return out
}
