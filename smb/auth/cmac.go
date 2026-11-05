package auth

import "crypto/aes"

// cmacDouble multiplies a CMAC subkey by x in GF(2^128).
func cmacDouble(v [16]byte) (out [16]byte) {
	for i := range 15 {
		out[i] = v[i]<<1 | v[i+1]>>7
	}
	out[15] = v[15] << 1
	if v[0]&0x80 != 0 {
		out[15] ^= 0x87
	}
	return
}

// SignAESCMAC computes the RFC 4493 AES-CMAC authentication tag.
func SignAESCMAC(key, msg []byte) [16]byte {
	c, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	var zero, l, state [16]byte
	c.Encrypt(l[:], zero[:])
	k1 := cmacDouble(l)
	k2 := cmacDouble(k1)
	for len(msg) > 16 {
		for i := range state {
			state[i] ^= msg[i]
		}
		c.Encrypt(state[:], state[:])
		msg = msg[16:]
	}
	var last [16]byte
	copy(last[:], msg)
	sub := k1
	if len(msg) < 16 {
		last[len(msg)] = 0x80
		sub = k2
	}
	for i := range state {
		state[i] ^= last[i] ^ sub[i]
	}
	c.Encrypt(state[:], state[:])
	return state
}
