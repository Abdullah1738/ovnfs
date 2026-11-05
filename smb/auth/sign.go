package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
)

// SignHMACSHA256 returns the first 16 bytes of HMAC-SHA256 for SMB2 signing.
func SignHMACSHA256(key, msg []byte) (out [16]byte) {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	copy(out[:], h.Sum(nil))
	return
}

// SignAESGMAC authenticates msg as GCM associated data without plaintext.
func SignAESGMAC(key []byte, nonce [12]byte, msg []byte) (out [16]byte) {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	copy(out[:], gcm.Seal(nil, nonce[:], nil, msg))
	return
}
