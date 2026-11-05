package auth

import (
	"bytes"
	"errors"
)

// spnegoOID identifies the SPNEGO GSS mechanism.
var spnegoOID = []byte{0x2b, 6, 1, 5, 5, 2}

// ntlmOID identifies Microsoft's NTLMSSP mechanism.
var ntlmOID = []byte{0x2b, 6, 1, 4, 1, 0x82, 0x37, 2, 2, 10}

// der encodes a definite-length DER element.
func der(tag byte, p []byte) []byte {
	out := []byte{tag}
	n := len(p)
	if n < 128 {
		out = append(out, byte(n))
	} else {
		var tmp [8]byte
		i := 8
		for n > 0 {
			i--
			tmp[i] = byte(n)
			n >>= 8
		}
		out = append(out, 0x80|byte(8-i))
		out = append(out, tmp[i:]...)
	}
	return append(out, p...)
}

// derRead consumes one bounded DER element, rejecting indefinite lengths.
func derRead(p []byte) (tag byte, value, rest []byte, err error) {
	if len(p) < 2 {
		err = errors.New("short DER")
		return
	}
	tag = p[0]
	n := int(p[1])
	pos := 2
	if n&128 != 0 {
		count := n & 127
		if count == 0 || count > 4 || len(p) < 2+count {
			err = errors.New("invalid DER length")
			return
		}
		n = 0
		for _, b := range p[2 : 2+count] {
			n = n<<8 | int(b)
		}
		pos += count
		if n < 128 {
			err = errors.New("nonminimal DER length")
			return
		}
	}
	if n > len(p)-pos {
		err = errors.New("truncated DER")
		return
	}
	value = p[pos : pos+n]
	rest = p[pos+n:]
	return
}

// NegotiateHint advertises NTLMSSP with the Windows-compatible SPNEGO hint.
func NegotiateHint() []byte {
	mechs := der(0xa0, der(0x30, der(6, ntlmOID)))
	hint := der(0xa3, der(0x30, der(0xa0, der(0x1b, []byte("not_defined_in_RFC4178@please_ignore")))))
	return der(0x60, append(der(6, spnegoOID), der(0xa0, der(0x30, append(mechs, hint...)))...))
}

// unwrapToken extracts NTLM and an optional SPNEGO mechanism-list MIC.
func unwrapToken(in []byte) (token, mic, mechs []byte, raw bool, err error) {
	if bytes.HasPrefix(in, []byte("NTLMSSP\x00")) {
		return in, nil, nil, true, nil
	}
	tag, p, rest, err := derRead(in)
	if err != nil {
		return
	}
	if len(rest) != 0 {
		err = errors.New("trailing SPNEGO data")
		return
	}
	if tag == 0x60 {
		var oid []byte
		tag, oid, p, err = derRead(p)
		if err != nil {
			return
		}
		if tag != 6 || !bytes.Equal(oid, spnegoOID) {
			err = errors.New("unsupported GSS mechanism")
			return
		}
		tag, p, rest, err = derRead(p)
		if err != nil {
			return
		}
		if len(rest) != 0 {
			err = errors.New("trailing GSS data")
			return
		}
	}
	if tag != 0xa0 && tag != 0xa1 {
		err = errors.New("invalid SPNEGO token")
		return
	}
	initial := tag == 0xa0
	tag, p, rest, err = derRead(p)
	if err != nil {
		return
	}
	if tag != 0x30 || len(rest) != 0 {
		err = errors.New("invalid SPNEGO sequence")
		return
	}
	for len(p) > 0 {
		var v []byte
		tag, v, p, err = derRead(p)
		if err != nil {
			return
		}
		inner, value, trailing, e := derRead(v)
		if e != nil {
			err = e
			return
		}
		if len(trailing) != 0 {
			err = errors.New("invalid SPNEGO field")
			return
		}
		switch {
		case initial && tag == 0xa0:
			if inner != 0x30 {
				err = errors.New("invalid mechanism list")
				return
			}
			mechs = append([]byte(nil), v...)
			found := false
			for len(value) > 0 {
				var oid []byte
				inner, oid, value, err = derRead(value)
				if err != nil {
					return
				}
				if inner != 6 {
					err = errors.New("invalid mechanism OID")
					return
				}
				found = found || bytes.Equal(oid, ntlmOID)
			}
			if !found {
				err = errors.New("NTLM not offered")
				return
			}
		case tag == 0xa2:
			if inner != 4 {
				err = errors.New("invalid mechanism token")
				return
			}
			token = value
		case tag == 0xa3:
			if inner != 4 {
				err = errors.New("invalid mechanism MIC")
				return
			}
			mic = value
		}
	}
	if len(token) == 0 {
		err = errors.New("missing NTLM token")
	}
	return
}

// wrapResponse encodes SPNEGO completion state and the optional NTLM token.
func wrapResponse(token []byte, state byte, first bool) []byte {
	p := der(0xa0, der(0x0a, []byte{state}))
	if first {
		p = append(p, der(0xa1, der(6, ntlmOID))...)
	}
	if len(token) > 0 {
		p = append(p, der(0xa2, der(4, token))...)
	}
	return der(0xa1, der(0x30, p))
}
