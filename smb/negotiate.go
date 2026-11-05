package smb

import (
	"bytes"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"time"

	"github.com/stencil-hq/ovnfs/ntinfo"
	"github.com/stencil-hq/ovnfs/smb/auth"
)

// statusNoPreauthIntegrityHashOverlap means no offered preauthentication hash is supported.
const statusNoPreauthIntegrityHashOverlap uint32 = 0xc05d0000

// smbServerStart records the lifetime of this server process for negotiation replies.
var smbServerStart = time.Now()

// smbPosixGUID identifies the POSIX extensions defined by the SMB3 UNIX extensions specification.
var smbPosixGUID = [16]byte{0x93, 0xad, 0x25, 0x50, 0x9c, 0xb4, 0x11, 0xe7, 0xb4, 0x23, 0x83, 0xde, 0x96, 0x8b, 0xcd, 0x7c}

// init registers negotiation before the connection dispatches requests.
func init() {
	handlers[cmdNegotiate] = handleNegotiate
}

// preauthDigest extends an SMB 3.1.1 preauthentication transcript with one complete SMB message.
func preauthDigest(previous [64]byte, msg []byte) [64]byte {
	h := sha512.New()
	_, _ = h.Write(previous[:])
	_, _ = h.Write(msg)
	var next [64]byte
	h.Sum(next[:0])
	return next
}

// negotiateContext appends one aligned SMB 3.1.1 negotiate context.
func negotiateContext(dst []byte, kind uint16, data []byte) []byte {
	for (64+len(dst))&7 != 0 {
		dst = append(dst, 0)
	}
	dst = binary.LittleEndian.AppendUint16(dst, kind)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(data)))
	dst = binary.LittleEndian.AppendUint32(dst, 0)
	return append(dst, data...)
}

// negotiateReply builds the fixed negotiation reply and the advertised security mechanism.
func negotiateReply(c *conn, dialect uint16) []byte {
	hint := auth.NegotiateHint()
	body := make([]byte, 64, 64+len(hint)+96)
	binary.LittleEndian.PutUint16(body[0:2], 65)
	binary.LittleEndian.PutUint16(body[2:4], c.serverSecMode)
	binary.LittleEndian.PutUint16(body[4:6], dialect)
	copy(body[8:24], c.srv.guid[:])
	binary.LittleEndian.PutUint32(body[24:28], c.serverCaps)
	binary.LittleEndian.PutUint32(body[28:32], c.maxTrans)
	binary.LittleEndian.PutUint32(body[32:36], c.maxRead)
	binary.LittleEndian.PutUint32(body[36:40], c.maxWrite)
	binary.LittleEndian.PutUint64(body[40:48], ntinfo.Filetime(time.Now()))
	binary.LittleEndian.PutUint64(body[48:56], ntinfo.Filetime(smbServerStart))
	binary.LittleEndian.PutUint16(body[56:58], 128)
	binary.LittleEndian.PutUint16(body[58:60], uint16(len(hint)))
	return append(body, hint...)
}

// negotiateLimits sets only capabilities implemented by this server.
func negotiateLimits(c *conn, dialect uint16) {
	c.maxRead, c.maxWrite, c.maxTrans = 8<<20, 8<<20, 1<<20
	c.serverSecMode = 1
	if len(c.srv.Users) != 0 {
		c.serverSecMode |= 2
	}
	c.serverCaps = 0
	if dialect >= 0x0210 {
		c.serverCaps = 6 // SMB2_GLOBAL_CAP_LEASING | SMB2_GLOBAL_CAP_LARGE_MTU.
	}
	c.signingAlg = 0
	if dialect >= 0x0300 {
		c.signingAlg = 1
	}
}

// handleNegotiate chooses the highest common dialect and validates 3.1.1 integrity contexts.
func handleNegotiate(r *request) ([]byte, uint32) {
	b := r.body
	if len(b) < 36 || binary.LittleEndian.Uint16(b[:2]) != 36 || r.c.dialect != 0 {
		return nil, statusInvalidParameter
	}
	n := int(binary.LittleEndian.Uint16(b[2:4]))
	if n == 0 || n > (len(b)-36)/2 {
		return nil, statusInvalidParameter
	}
	var selected uint16
	dialects := make([]uint16, n)
	for i := range dialects {
		d := binary.LittleEndian.Uint16(b[36+2*i:])
		dialects[i] = d
		switch d {
		case 0x0202, 0x0210, 0x0300, 0x0302, 0x0311:
			if d > selected {
				selected = d
			}
		}
	}
	if selected == 0 {
		return nil, statusNotSupported
	}
	var sha512Offered, signingContext, posix bool
	var signingAlgorithm uint16 = 1
	if selected == 0x0311 {
		count := int(binary.LittleEndian.Uint16(b[32:34]))
		offset := uint64(binary.LittleEndian.Uint32(b[28:32]))
		if count == 0 || offset&7 != 0 || offset < uint64(64+36+2*n) || offset > uint64(64+len(b)) {
			return nil, statusInvalidParameter
		}
		cursor := int(offset) - 64
		seen := make(map[uint16]bool, count)
		for range count {
			if cursor > len(b)-8 {
				return nil, statusInvalidParameter
			}
			kind := binary.LittleEndian.Uint16(b[cursor:])
			length := int(binary.LittleEndian.Uint16(b[cursor+2:]))
			if length > len(b)-cursor-8 || seen[kind] {
				return nil, statusInvalidParameter
			}
			seen[kind] = true
			data := b[cursor+8 : cursor+8+length]
			switch kind {
			case 1: // SMB2_PREAUTH_INTEGRITY_CAPABILITIES.
				if len(data) < 4 {
					return nil, statusInvalidParameter
				}
				hashes := int(binary.LittleEndian.Uint16(data[:2]))
				salt := int(binary.LittleEndian.Uint16(data[2:4]))
				if hashes > (len(data)-4)/2 || salt > len(data)-4-2*hashes {
					return nil, statusInvalidParameter
				}
				for j := range hashes {
					sha512Offered = sha512Offered || binary.LittleEndian.Uint16(data[4+2*j:]) == 1
				}
			case 8: // SMB2_SIGNING_CAPABILITIES.
				if len(data) < 2 {
					return nil, statusInvalidParameter
				}
				num := int(binary.LittleEndian.Uint16(data[:2]))
				if num == 0 || num > (len(data)-2)/2 {
					return nil, statusInvalidParameter
				}
				var cmac, gmac bool
				for j := range num {
					switch binary.LittleEndian.Uint16(data[2+2*j:]) {
					case 1:
						cmac = true
					case 2:
						gmac = true
					}
				}
				if !cmac && !gmac {
					return nil, statusNotSupported
				}
				signingContext = true
				if gmac {
					signingAlgorithm = 2
				}
			case 0x0100: // SMB2_POSIX_EXTENSIONS_AVAILABLE.
				posix = bytes.Equal(data, smbPosixGUID[:])
			}
			cursor = (cursor + 8 + length + 7) &^ 7
		}
		if !sha512Offered {
			return nil, statusNoPreauthIntegrityHashOverlap
		}
	}
	c := r.c
	c.dialect = selected
	c.clientSecMode = binary.LittleEndian.Uint16(b[4:6])
	c.clientCaps = binary.LittleEndian.Uint32(b[8:12])
	copy(c.clientGUID[:], b[12:28])
	c.clientDialects = dialects
	negotiateLimits(c, selected)
	c.posix = posix
	if c.srv.Logf != nil {
		c.srv.Logf("smb %s negotiate dialect=%#04x client_security=%#04x server_security=%#04x capabilities=%#08x", c.nc.RemoteAddr(), selected, c.clientSecMode, c.serverSecMode, c.serverCaps)
	}
	body := negotiateReply(c, selected)
	if selected == 0x0311 {
		c.signingAlg = signingAlgorithm
		data := make([]byte, 38)
		binary.LittleEndian.PutUint16(data[0:2], 1)
		binary.LittleEndian.PutUint16(data[2:4], 32)
		binary.LittleEndian.PutUint16(data[4:6], 1)
		if _, err := rand.Read(data[6:]); err != nil {
			c.dialect = 0
			return nil, statusInternalError
		}
		contextOffset := (64 + len(body) + 7) &^ 7
		binary.LittleEndian.PutUint32(body[60:64], uint32(contextOffset))
		body = negotiateContext(body, 1, data)
		count := uint16(1)
		if signingContext {
			signing := []byte{1, 0, byte(signingAlgorithm), 0}
			body = negotiateContext(body, 8, signing)
			count++
		}
		if posix {
			body = negotiateContext(body, 0x0100, smbPosixGUID[:])
			count++
		}
		binary.LittleEndian.PutUint16(body[6:8], count)
		c.preauth = preauthDigest([64]byte{}, r.raw)
		r.onSent = func(msg []byte) { c.preauth = preauthDigest(c.preauth, msg) }
	}
	return body, statusSuccess
}

// handleSMB1Negotiate supports the SMB1 multi-protocol bootstrap without accepting SMB1 sessions.
func handleSMB1Negotiate(c *conn, msg []byte) []byte {
	if len(msg) < 35 || !bytes.Equal(msg[:4], []byte{0xff, 'S', 'M', 'B'}) || msg[4] != 0x72 || msg[32] != 0 {
		return nil
	}
	length := int(binary.LittleEndian.Uint16(msg[33:35]))
	if length > len(msg)-35 {
		return nil
	}
	list := msg[35 : 35+length]
	var dialect uint16
	for len(list) != 0 {
		if list[0] != 2 {
			return nil
		}
		end := bytes.IndexByte(list[1:], 0)
		if end < 0 {
			return nil
		}
		name := string(list[1 : 1+end])
		if name == "SMB 2.???" {
			dialect = 0x02ff
		} else if name == "SMB 2.002" && dialect == 0 {
			dialect = 0x0202
		}
		list = list[end+2:]
	}
	if dialect == 0 {
		return nil
	}
	negotiateLimits(c, dialect)
	if dialect == 0x0202 {
		c.dialect = dialect
	}
	h := header{Command: cmdNegotiate, Flags: flagResponse, Credits: 1}
	return append(h.appendTo(nil), negotiateReply(c, dialect)...)
}
