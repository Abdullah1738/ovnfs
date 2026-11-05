package auth

import (
	"bytes"
	"crypto/rc4"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// unhex decodes literal known-answer data for these tests.
func unhex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// TestMD4 checks all RFC 1320 appendix A.5 vectors.
func TestMD4(t *testing.T) {
	vectors := [][2]string{{"", "31d6cfe0d16ae931b73c59d7e0c089c0"}, {"a", "bde52cb31de33e46245e05fbdbd6fb24"}, {"abc", "a448017aaf21d8525fc10ae87aa6729d"}, {"message digest", "d9130a8164549fe818874806e1c7014b"}, {"abcdefghijklmnopqrstuvwxyz", "d79e1c308aa5bbcdeea8ed63df412da9"}, {"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789", "043f8582f241db351ce627e153e7f0e4"}, {"12345678901234567890123456789012345678901234567890123456789012345678901234567890", "e33b4ddc9c38f2199c3e7b164fcc0536"}}
	for _, v := range vectors {
		got := MD4([]byte(v[0]))
		if !bytes.Equal(got[:], unhex(v[1])) {
			t.Errorf("MD4(%q) = %x", v[0], got)
		}
	}
}

// TestCMAC checks RFC 4493 section 4's four message lengths.
func TestCMAC(t *testing.T) {
	key := unhex("2b7e151628aed2a6abf7158809cf4f3c")
	msg := unhex("6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411e5fbc1191a0a52eff69f2445df4f9b17ad2b417be66c3710")
	for _, v := range []struct {
		n    int
		want string
	}{{0, "bb1d6929e95937287fa37d129b756746"}, {16, "070a16b46b4d4144f79bdd9dd04a287c"}, {40, "dfa66747de9ae63030ca32611497c827"}, {64, "51f0bebf7e3b9d92fc49741779363cfe"}} {
		got := SignAESCMAC(key, msg[:v.n])
		if !bytes.Equal(got[:], unhex(v.want)) {
			t.Errorf("CMAC(%d) = %x", v.n, got)
		}
	}
}

// TestNTLMv2Vectors checks MS-NLMP section 4.2.4's password, proof and key.
func TestNTLMv2Vectors(t *testing.T) {
	key := ntlmV2Key("Password", "User", "Domain")
	if !bytes.Equal(key, unhex("0c868a403bfd7a93a3001ef22ef02e3f")) {
		t.Fatalf("NTOWFv2 = %x", key)
	}
	blob := unhex("01010000000000000000000000000000aaaaaaaaaaaaaaaa0000000002000c0044006f006d00610069006e0001000c005300650072007600650072000000000000000000")
	proof := ntlmHMAC(key, unhex("0123456789abcdef"), blob)
	if !bytes.Equal(proof, unhex("68cd0ab851e51c96aabc927bebef6a1c")) {
		t.Fatalf("NTProofStr = %x", proof)
	}
	base := ntlmHMAC(key, proof)
	if !bytes.Equal(base, unhex("8de40ccadbc14a82f15cb0ad0de95ca3")) {
		t.Fatalf("SessionBaseKey = %x", base)
	}
}

// testAuthenticate is a small NTLMv2 client with key exchange and transcript MIC.
func testAuthenticate(negotiate, challenge []byte, password string) ([]byte, []byte) {
	flags := binary.LittleEndian.Uint32(challenge[20:])
	av, _ := ntlmField(challenge, 40)
	blob := make([]byte, 28)
	blob[0] = 1
	blob[1] = 1
	copy(blob[16:], []byte("client12"))
	blob = append(blob, av...)
	blob = append(blob, 0, 0, 0, 0)
	responseKey := ntlmV2Key(password, "User", "Domain")
	proof := ntlmHMAC(responseKey, challenge[24:32], blob)
	nt := append(append([]byte(nil), proof...), blob...)
	base := ntlmHMAC(responseKey, proof)
	exported := []byte("0123456789abcdef")
	encrypted := make([]byte, 16)
	stream, _ := rc4.NewCipher(base)
	stream.XORKeyStream(encrypted, exported)
	p := make([]byte, 88)
	copy(p, "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(p[8:], 3)
	binary.LittleEndian.PutUint32(p[60:], flags)
	copy(p[64:72], []byte{10, 0, 0, 0, 0, 0, 0, 15})
	values := [][]byte{make([]byte, 24), nt, ntlmUTF16("Domain"), ntlmUTF16("User"), ntlmUTF16("CLIENT"), encrypted}
	for i, v := range values {
		p = ntlmPutField(p, 12+i*8, v)
	}
	copy(p[72:88], ntlmHMAC(exported, negotiate, challenge, p))
	return p, exported
}

// TestExchange exercises raw and SPNEGO authentication, password failure and guest access.
func TestExchange(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		for _, mode := range []string{"success", "wrong", "guest"} {
			t.Run(mode+map[bool]string{false: "/raw", true: "/spnego"}[wrapped], func(t *testing.T) {
				server := &Server{Users: map[string]string{"user": "Password"}, Domain: "Domain"}
				if mode == "guest" {
					server.Users = nil
				}
				negotiate := make([]byte, 40)
				copy(negotiate, "NTLMSSP\x00")
				binary.LittleEndian.PutUint32(negotiate[8:], 1)
				binary.LittleEndian.PutUint32(negotiate[12:], ntlmFlags|0x10)
				input := negotiate
				if wrapped {
					fields := append(der(0xa0, der(0x30, der(6, ntlmOID))), der(0xa2, der(4, negotiate))...)
					input = der(0x60, append(der(6, spnegoOID), der(0xa0, der(0x30, fields))...))
				}
				var exchange Exchange
				output, done, err := exchange.Step(server, input)
				if err != nil || done {
					t.Fatalf("negotiate: done=%v err=%v", done, err)
				}
				challenge := output
				if wrapped {
					challenge, _, _, _, err = unwrapToken(output)
					if err != nil {
						t.Fatal(err)
					}
				}
				password := "Password"
				if mode == "wrong" {
					password = "incorrect"
				}
				authenticate, key := testAuthenticate(negotiate, challenge, password)
				input = authenticate
				if wrapped {
					input = der(0xa1, der(0x30, der(0xa2, der(4, authenticate))))
				}
				_, done, err = exchange.Step(server, input)
				if mode == "wrong" {
					if err == nil {
						t.Fatal("wrong password accepted")
					}
					return
				}
				if err != nil || !done {
					t.Fatalf("authenticate: done=%v err=%v", done, err)
				}
				result := exchange.Result()
				if mode == "guest" {
					if !result.Guest || len(result.SessionKey) != 0 {
						t.Fatalf("guest result: %+v", result)
					}
				} else if result.Guest || !bytes.Equal(result.SessionKey, key) {
					t.Fatalf("authenticated result: %+v", result)
				}
			})
		}
	}
}
