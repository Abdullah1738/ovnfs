package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rc4"
	"encoding/binary"
	"errors"
	"strings"
	"time"
	"unicode/utf16"
)

// Server supplies the configured NTLM identities and advertised domain.
type Server struct {
	Users  map[string]string
	Domain string
}

// Result is the authenticated identity and exported SMB session key.
type Result struct {
	User, Domain     string
	Guest, Anonymous bool
	SessionKey       []byte
}

// Exchange retains the challenge and transcript for one NTLM authentication.
type Exchange struct {
	stage                int
	raw                  bool
	negotiate, challenge []byte
	nonce                [8]byte
	flags                uint32
	result               Result
}

// ntlmFlags are the server capabilities, excluding the client's optional SIGN bit.
const ntlmFlags uint32 = 0x00000001 | 0x00000200 | 0x00008000 | 0x00080000 | 0x00800000 | 0x20000000 | 0x80000000 | 0x40000000 | 0x02000000 | 0x00020000 | 0x00000004

// Result returns the identity after a successful completed exchange.
func (e *Exchange) Result() Result { return e.result }

// ntlmUTF16 encodes strings into the NTLM wire representation.
func ntlmUTF16(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, len(u)*2)
	for i, v := range u {
		binary.LittleEndian.PutUint16(out[i*2:], v)
	}
	return out
}

// ntlmString decodes a bounded Unicode NTLM field.
func ntlmString(b []byte) (string, error) {
	if len(b)%2 != 0 {
		return "", errors.New("odd NTLM Unicode length")
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u)), nil
}

// ntlmHMAC computes NTLM's HMAC-MD5 primitive.
func ntlmHMAC(key []byte, parts ...[]byte) []byte {
	h := hmac.New(md5.New, key)
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// ntlmV2Key derives the response key for a supplied username and domain.
func ntlmV2Key(password, user, domain string) []byte {
	hash := MD4(ntlmUTF16(password))
	return ntlmHMAC(hash[:], ntlmUTF16(strings.ToUpper(user)+domain))
}

// ntlmField bounds-checks an NTLM security buffer against its complete packet.
func ntlmField(p []byte, pos int) ([]byte, error) {
	if pos < 0 || len(p) < pos+8 {
		return nil, errors.New("short NTLM security buffer")
	}
	n := int(binary.LittleEndian.Uint16(p[pos:]))
	max := int(binary.LittleEndian.Uint16(p[pos+2:]))
	off := uint64(binary.LittleEndian.Uint32(p[pos+4:]))
	if max < n || off > uint64(len(p)) || uint64(n) > uint64(len(p))-off {
		return nil, errors.New("invalid NTLM security buffer")
	}
	return p[int(off) : int(off)+n], nil
}

// ntlmPutField appends a payload and records its security buffer.
func ntlmPutField(p []byte, pos int, value []byte) []byte {
	binary.LittleEndian.PutUint16(p[pos:], uint16(len(value)))
	binary.LittleEndian.PutUint16(p[pos+2:], uint16(len(value)))
	binary.LittleEndian.PutUint32(p[pos+4:], uint32(len(p)))
	return append(p, value...)
}

// ntlmAV appends one target-information attribute.
func ntlmAV(dst []byte, id uint16, value []byte) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, id)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(value)))
	return append(dst, value...)
}

// Step processes a negotiate or authenticate token, rejecting reused exchanges.
func (e *Exchange) Step(s *Server, in []byte) (out []byte, done bool, err error) {
	fail := func(cause error) ([]byte, bool, error) {
		e.stage = 3
		if !e.raw {
			return wrapResponse(nil, 2, false), true, cause
		}
		return nil, true, cause
	}
	token, _, _, raw, err := unwrapToken(in)
	if err != nil {
		return fail(err)
	}
	if len(token) < 12 || !bytes.Equal(token[:8], []byte("NTLMSSP\x00")) {
		return fail(errors.New("invalid NTLM signature"))
	}
	kind := binary.LittleEndian.Uint32(token[8:])
	if e.stage == 0 {
		e.raw = raw
		if kind != 1 || len(token) < 32 {
			return fail(errors.New("expected NTLM negotiate"))
		}
		flags := binary.LittleEndian.Uint32(token[12:])
		if flags&1 == 0 {
			return fail(errors.New("NTLM Unicode required"))
		}
		if _, err = ntlmField(token, 16); err != nil {
			return fail(err)
		}
		if _, err = ntlmField(token, 24); err != nil {
			return fail(err)
		}
		e.negotiate = append([]byte(nil), token...)
		e.flags = ntlmFlags | flags&0x10
		// Key exchange is negotiated only when the client supports it.
		e.flags &^= (0x40000000 | 0x02000000) &^ flags
		if _, err = rand.Read(e.nonce[:]); err != nil {
			return fail(err)
		}
		domain := s.Domain
		if domain == "" {
			domain = "WORKGROUP"
		}
		computer := "OVNFS"
		var av []byte
		av = ntlmAV(av, 2, ntlmUTF16(domain))
		av = ntlmAV(av, 1, ntlmUTF16(computer))
		av = ntlmAV(av, 4, ntlmUTF16(strings.ToLower(domain)))
		av = ntlmAV(av, 3, ntlmUTF16(strings.ToLower(computer)))
		var stamp [8]byte
		binary.LittleEndian.PutUint64(stamp[:], uint64(time.Now().UnixNano()/100)+116444736000000000)
		av = ntlmAV(av, 7, stamp[:])
		av = ntlmAV(av, 0, nil)
		p := make([]byte, 56)
		copy(p, "NTLMSSP\x00")
		binary.LittleEndian.PutUint32(p[8:], 2)
		binary.LittleEndian.PutUint32(p[20:], e.flags)
		copy(p[24:], e.nonce[:])
		copy(p[48:], []byte{10, 0, 0, 0, 0, 0, 0, 15})
		p = ntlmPutField(p, 12, ntlmUTF16(domain))
		p = ntlmPutField(p, 40, av)
		e.challenge = p
		e.stage = 1
		if raw {
			return append([]byte(nil), p...), false, nil
		}
		return wrapResponse(p, 1, true), false, nil
	}
	if e.stage != 1 || kind != 3 || raw != e.raw {
		return fail(errors.New("unexpected NTLM authenticate"))
	}
	e.stage = 3
	if len(token) < 64 {
		return fail(errors.New("short NTLM authenticate"))
	}
	fields := [6][]byte{}
	for i := range fields {
		fields[i], err = ntlmField(token, 12+i*8)
		if err != nil {
			return fail(err)
		}
	}
	domain, err := ntlmString(fields[2])
	if err != nil {
		return fail(err)
	}
	user, err := ntlmString(fields[3])
	if err != nil {
		return fail(err)
	}
	if user == "" && len(fields[1]) == 0 && (len(fields[0]) == 0 || (len(fields[0]) == 1 && fields[0][0] == 0)) {
		e.result = Result{Anonymous: true}
		e.stage = 2
	} else if len(s.Users) == 0 {
		e.result = Result{User: user, Domain: domain, Guest: true}
		e.stage = 2
	} else {
		password := ""
		found := false
		for name, pw := range s.Users {
			if strings.EqualFold(name, user) {
				password = pw
				found = true
				break
			}
		}
		if !found {
			return fail(errors.New("unknown NTLM user"))
		}
		nt := fields[1]
		if len(nt) < 48 || nt[16] != 1 || nt[17] != 1 || !bytes.Equal(nt[18:24], make([]byte, 6)) {
			return fail(errors.New("NTLMv2 required"))
		}
		blob := nt[16:]
		micRequired := false
		av := blob[28:]
		terminated := false
		for len(av) >= 4 {
			id := binary.LittleEndian.Uint16(av)
			n := int(binary.LittleEndian.Uint16(av[2:]))
			av = av[4:]
			if n > len(av) {
				return fail(errors.New("truncated NTLMv2 AV pair"))
			}
			if id == 0 {
				if n != 0 {
					return fail(errors.New("invalid NTLMv2 AV terminator"))
				}
				terminated = true
				break
			}
			if id == 6 && n == 4 {
				micRequired = binary.LittleEndian.Uint32(av)&2 != 0
			}
			av = av[n:]
		}
		if !terminated {
			return fail(errors.New("unterminated NTLMv2 target info"))
		}
		var base []byte
		for _, candidate := range []string{domain, ""} {
			key := ntlmV2Key(password, user, candidate)
			expected := ntlmHMAC(key, e.nonce[:], blob)
			if hmac.Equal(expected, nt[:16]) {
				base = ntlmHMAC(key, nt[:16])
				break
			}
		}
		if base == nil {
			return fail(errors.New("NTLMv2 proof mismatch"))
		}
		flags := binary.LittleEndian.Uint32(token[60:])
		key := base
		if e.flags&flags&0x40000000 != 0 {
			if len(fields[5]) != 16 {
				return fail(errors.New("invalid encrypted NTLM session key"))
			}
			cipher, rcerr := rc4.NewCipher(base)
			if rcerr != nil {
				return fail(rcerr)
			}
			key = make([]byte, 16)
			cipher.XORKeyStream(key, fields[5])
		}
		micPos := 64
		if flags&0x02000000 != 0 {
			micPos = 72
		}
		firstPayload := len(token)
		for i := range fields {
			if len(fields[i]) > 0 {
				off := int(binary.LittleEndian.Uint32(token[16+i*8:]))
				if off < firstPayload {
					firstPayload = off
				}
			}
		}
		hasMIC := firstPayload >= micPos+16 && len(token) >= micPos+16
		if micRequired && !hasMIC {
			return fail(errors.New("missing NTLM MIC"))
		}
		if hasMIC {
			packet := append([]byte(nil), token...)
			clear(packet[micPos : micPos+16])
			expected := ntlmHMAC(key, e.negotiate, e.challenge, packet)
			if !hmac.Equal(expected, token[micPos:micPos+16]) {
				return fail(errors.New("NTLM MIC mismatch"))
			}
		}
		e.result = Result{User: user, Domain: domain, SessionKey: key}
		e.stage = 2
	}
	if e.raw {
		return nil, true, nil
	}
	return wrapResponse(nil, 0, false), true, nil
}
