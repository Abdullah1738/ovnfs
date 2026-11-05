package smb

import (
	"encoding/binary"
	"os"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/stencil-hq/ovnfs/ntinfo"
)

// utf16Decode decodes little-endian UTF-16, replacing malformed surrogates.
func utf16Decode(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u))
}

// utf16Append appends little-endian UTF-16 without an implicit terminator.
func utf16Append(dst []byte, s string) []byte {
	for _, r := range s {
		if r <= 0xffff {
			dst = binary.LittleEndian.AppendUint16(dst, uint16(r))
			continue
		}
		hi, lo := utf16.EncodeRune(r)
		dst = binary.LittleEndian.AppendUint16(dst, uint16(hi))
		dst = binary.LittleEndian.AppendUint16(dst, uint16(lo))
	}
	return dst
}

// utf16Len returns the number of bytes needed to encode s as UTF-16.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xffff {
			n += 4
		} else {
			n += 2
		}
	}
	return n
}

// smbPath converts an SMB pathname without permitting traversal or named streams.
func smbPath(name string) (string, uint32) {
	if !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
		return "", statusObjectNameInvalid
	}
	name = strings.ReplaceAll(name, "\\", "/")
	parts := strings.Split(name, "/")
	clean := parts[:0]
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			return "", statusObjectNameInvalid
		}
		if i := strings.IndexByte(p, ':'); i >= 0 {
			stream := p[i:]
			if !strings.EqualFold(stream, ":$DATA") && !strings.EqualFold(stream, "::$DATA") {
				return "", statusObjectNameNotFound
			}
			p = p[:i]
			if p == "" || p == "." || p == ".." {
				return "", statusObjectNameInvalid
			}
		}
		if strings.ContainsAny(p, "\"<>|*?") {
			return "", statusObjectNameInvalid
		}
		for _, r := range p {
			if r < 32 {
				return "", statusObjectNameInvalid
			}
		}
		clean = append(clean, p)
	}
	return strings.Join(clean, "/"), statusSuccess
}

// fileAttributes keeps Unix write permissions separate from DOS read-only immutability.
func (c *conn) fileAttributes(info os.FileInfo) uint32 {
	attrs := ntinfo.Attributes(info)
	if c.posix || c.appleReadDir.Load() {
		attrs &^= 1
	}
	return attrs
}
