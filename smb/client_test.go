package smb

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/stencil-hq/ovnfs/smb/auth"
)

// wire protocol constants keep the test client independent of server implementation details.
const (
	wireSuccess        uint32 = 0
	wireMoreProcessing uint32 = 0xc0000016
	wireNoMoreFiles    uint32 = 0x80000006
	wireCollision      uint32 = 0xc0000035
	wireBadShare       uint32 = 0xc00000cc
	wireNotEmpty       uint32 = 0xc0000101
	wireLockDenied     uint32 = 0xc0000055
	wireLogonFailure   uint32 = 0xc000006d
	wireEOF            uint32 = 0xc0000011
	wireAccessDenied   uint32 = 0xc0000022
	wireRelated        uint32 = 4
	wireSigned         uint32 = 8
)

// wireID contains the two opaque values returned by CREATE.
type wireID [16]byte

// wireReply is a decoded SMB2 envelope with the original bytes retained for signature checks.
type wireReply struct {
	raw, body []byte
	status    uint32
	command   uint16
	flags     uint32
	tree      uint32
	session   uint64
}

// wireClient implements only the synchronous protocol operations exercised by these tests.
type wireClient struct {
	t               *testing.T
	conn            net.Conn
	message         uint64
	session         uint64
	tree            uint32
	dialect         uint16
	key             []byte
	preauth         [64]byte
	lastRequestHash [64]byte
	verified        int
}

// wireUTF16 encodes SMB names and NTLM identity fields without using server helpers.
func wireUTF16(s string) []byte {
	out := make([]byte, 0, 2*len(s))
	for _, u := range utf16.Encode([]rune(s)) {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

// wireString decodes a UTF-16LE name returned by the server.
func wireString(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u))
}

// newWireClient connects to an already-listening test server and bounds every network operation.
func newWireClient(t *testing.T, address string) *wireClient {
	t.Helper()
	nc, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	return &wireClient{t: t, conn: nc}
}

// hashMessage implements the SMB3.1.1 chained SHA-512 preauthentication hash.
func (c *wireClient) hashMessage(msg []byte) {
	h := sha512.New()
	_, _ = h.Write(c.preauth[:])
	_, _ = h.Write(msg)
	copy(c.preauth[:], h.Sum(nil))
}

// packet constructs an SMB2 request header without depending on the server's encoders.
func (c *wireClient) packet(command uint16, body []byte, flags uint32, tree uint32, session uint64) []byte {
	b := make([]byte, 64, 64+len(body))
	copy(b, "\xfeSMB")
	binary.LittleEndian.PutUint16(b[4:], 64)
	binary.LittleEndian.PutUint16(b[6:], 1)
	binary.LittleEndian.PutUint16(b[12:], command)
	binary.LittleEndian.PutUint16(b[14:], 32)
	binary.LittleEndian.PutUint32(b[16:], flags)
	binary.LittleEndian.PutUint64(b[24:], c.message)
	c.message++
	binary.LittleEndian.PutUint32(b[32:], 0x1234)
	binary.LittleEndian.PutUint32(b[36:], tree)
	binary.LittleEndian.PutUint64(b[40:], session)
	return append(b, body...)
}

// signature uses the dialect's negotiated signing primitive.
func (c *wireClient) signature(msg []byte) [16]byte {
	if c.dialect < 0x0300 {
		return auth.SignHMACSHA256(c.key, msg)
	}
	return auth.SignAESCMAC(c.key, msg)
}

// sign writes a request signature after all compound offsets have been finalized.
func (c *wireClient) sign(msg []byte) {
	binary.LittleEndian.PutUint32(msg[16:], binary.LittleEndian.Uint32(msg[16:])|wireSigned)
	clear(msg[48:64])
	sig := c.signature(msg)
	copy(msg[48:64], sig[:])
}

// verify checks that authenticated responses are signed and unmodified.
func (c *wireClient) verify(r wireReply) {
	c.t.Helper()
	if r.flags&wireSigned == 0 {
		c.t.Fatalf("command %d response is unsigned", r.command)
	}
	msg := append([]byte(nil), r.raw...)
	got := append([]byte(nil), msg[48:64]...)
	clear(msg[48:64])
	want := c.signature(msg)
	if !hmac.Equal(got, want[:]) {
		c.t.Fatalf("command %d response has invalid signature", r.command)
	}
	c.verified++
}

// writeFrame emits an RFC1002 session message with complete-write handling.
func (c *wireClient) writeFrame(msg []byte) error {
	if err := c.conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return err
	}
	frame := []byte{0, byte(len(msg) >> 16), byte(len(msg) >> 8), byte(len(msg))}
	frame = append(frame, msg...)
	for len(frame) > 0 {
		n, err := c.conn.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}

// readFrame receives one bounded RFC1002 session message.
func (c *wireClient) readFrame() ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(c.conn, h[:]); err != nil {
		return nil, err
	}
	n := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
	if h[0] != 0 || n < 64 || n > 16<<20 {
		return nil, fmt.Errorf("invalid SMB frame %x, length %d", h, n)
	}
	b := make([]byte, n)
	_, err := io.ReadFull(c.conn, b)
	return b, err
}

// decodeWireReply validates a response envelope and exposes its payload.
func decodeWireReply(raw []byte) (wireReply, error) {
	if len(raw) < 64 || !bytes.Equal(raw[:4], []byte("\xfeSMB")) || binary.LittleEndian.Uint16(raw[4:]) != 64 {
		return wireReply{}, fmt.Errorf("invalid SMB2 response")
	}
	return wireReply{raw: raw, body: raw[64:], status: binary.LittleEndian.Uint32(raw[8:]), command: binary.LittleEndian.Uint16(raw[12:]), flags: binary.LittleEndian.Uint32(raw[16:]), tree: binary.LittleEndian.Uint32(raw[36:]), session: binary.LittleEndian.Uint64(raw[40:])}, nil
}

// exchange sends a standalone operation and tracks negotiation transcripts and signatures.
func (c *wireClient) exchange(command uint16, body []byte) wireReply {
	c.t.Helper()
	msg := c.packet(command, body, 0, c.tree, c.session)
	if len(c.key) != 0 {
		c.sign(msg)
	}
	if command <= 1 {
		c.hashMessage(msg)
		c.lastRequestHash = c.preauth
	}
	if err := c.writeFrame(msg); err != nil {
		c.t.Fatal(err)
	}
	raw, err := c.readFrame()
	if err != nil {
		c.t.Fatal(err)
	}
	r, err := decodeWireReply(raw)
	if err != nil {
		c.t.Fatal(err)
	}
	if r.command != command || binary.LittleEndian.Uint64(raw[24:]) != binary.LittleEndian.Uint64(msg[24:]) || r.flags&1 == 0 {
		c.t.Fatalf("response envelope does not match command %d", command)
	}
	if len(c.key) != 0 {
		c.verify(r)
	}
	if command <= 1 {
		c.hashMessage(raw)
	}
	return r
}

// expectStatus reports protocol status failures at the consumer call site.
func (c *wireClient) expectStatus(r wireReply, want uint32) {
	c.t.Helper()
	if r.status != want {
		c.t.Fatalf("command %d: status %#08x, want %#08x", r.command, r.status, want)
	}
}

// negotiate offers caller-selected dialects and the mandatory SMB3.1.1 preauth context plus POSIX and CMAC.
func (c *wireClient) negotiate(dialects []uint16) {
	c.t.Helper()
	b := make([]byte, 36)
	binary.LittleEndian.PutUint16(b, 36)
	binary.LittleEndian.PutUint16(b[2:], uint16(len(dialects)))
	binary.LittleEndian.PutUint16(b[4:], 1)
	binary.LittleEndian.PutUint32(b[8:], 4)
	if _, err := rand.Read(b[12:28]); err != nil {
		c.t.Fatal(err)
	}
	has311 := false
	for _, d := range dialects {
		b = binary.LittleEndian.AppendUint16(b, d)
		has311 = has311 || d == 0x0311
	}
	if has311 {
		for (64+len(b))%8 != 0 {
			b = append(b, 0)
		}
		binary.LittleEndian.PutUint32(b[28:], uint32(64+len(b)))
		binary.LittleEndian.PutUint16(b[32:], 3)
		preauth := []byte{1, 0, 32, 0, 1, 0}
		salt := make([]byte, 32)
		if _, err := rand.Read(salt); err != nil {
			c.t.Fatal(err)
		}
		preauth = append(preauth, salt...)
		b = wireContext(b, 1, preauth)
		b = wireContext(b, 8, []byte{1, 0, 1, 0})
		b = wireContext(b, 0x0100, []byte{0x93, 0xad, 0x25, 0x50, 0x9c, 0xb4, 0x11, 0xe7, 0xb4, 0x23, 0x83, 0xde, 0x96, 0x8b, 0xcd, 0x7c})
	}
	r := c.exchange(0, b)
	c.expectStatus(r, wireSuccess)
	if len(r.body) < 64 {
		c.t.Fatal("short NEGOTIATE response")
	}
	c.dialect = binary.LittleEndian.Uint16(r.body[4:])
	found := false
	for _, d := range dialects {
		found = found || d == c.dialect
	}
	if !found {
		c.t.Fatalf("server selected unoffered dialect %#x", c.dialect)
	}
}

// wireContext appends one aligned SMB3.1.1 negotiate context.
func wireContext(dst []byte, kind uint16, data []byte) []byte {
	for (64+len(dst))%8 != 0 {
		dst = append(dst, 0)
	}
	dst = binary.LittleEndian.AppendUint16(dst, kind)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(data)))
	dst = append(dst, 0, 0, 0, 0)
	return append(dst, data...)
}

// wireDER encodes the small definite-length DER values required for SPNEGO test tokens.
func wireDER(tag byte, body []byte) []byte {
	b := []byte{tag}
	if len(body) < 128 {
		b = append(b, byte(len(body)))
	} else if len(body) < 256 {
		b = append(b, 0x81, byte(len(body)))
	} else {
		b = append(b, 0x82, byte(len(body)>>8), byte(len(body)))
	}
	return append(b, body...)
}

// wireSPNEGO wraps an NTLM token in an initial or continuation GSS negotiation token.
func wireSPNEGO(token []byte, initial bool) []byte {
	tok := wireDER(0xa2, wireDER(4, token))
	if !initial {
		return wireDER(0xa1, wireDER(0x30, tok))
	}
	ntlmOID := []byte{6, 10, 0x2b, 6, 1, 4, 1, 0x82, 0x37, 2, 2, 10}
	mechs := wireDER(0xa0, wireDER(0x30, ntlmOID))
	inner := wireDER(0xa0, wireDER(0x30, append(mechs, tok...)))
	return wireDER(0x60, append([]byte{6, 6, 0x2b, 6, 1, 5, 5, 2}, inner...))
}

// wireSecurity extracts a security buffer from an absolute offset in an SMB response.
func (c *wireClient) wireSecurity(r wireReply, offset, length int) []byte {
	c.t.Helper()
	if offset < 64 || length < 0 || offset > len(r.raw) || length > len(r.raw)-offset {
		c.t.Fatal("invalid security buffer")
	}
	return r.raw[offset : offset+length]
}

// sessionSetup sends one raw or SPNEGO-wrapped NTLM token.
func (c *wireClient) sessionSetup(token []byte) wireReply {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint16(b, 25)
	b[3] = 1
	binary.LittleEndian.PutUint16(b[12:], 88)
	binary.LittleEndian.PutUint16(b[14:], uint16(len(token)))
	return c.exchange(1, append(b, token...))
}

// wireSecBuf appends one NTLM payload and installs its security buffer descriptor.
func wireSecBuf(msg []byte, field int, data []byte) []byte {
	binary.LittleEndian.PutUint16(msg[field:], uint16(len(data)))
	binary.LittleEndian.PutUint16(msg[field+2:], uint16(len(data)))
	binary.LittleEndian.PutUint32(msg[field+4:], uint32(len(msg)))
	return append(msg, data...)
}

// wireMD5 computes the HMAC-MD5 used by NTLMv2 proofs and session base keys.
func wireMD5(key, data []byte) []byte {
	h := hmac.New(md5.New, key)
	_, _ = h.Write(data)
	return h.Sum(nil)
}

// login performs a real NTLMv2 exchange, or anonymous NTLM when user is empty.
func (c *wireClient) login(user, password string, spnego bool) wireReply {
	c.t.Helper()
	const flags uint32 = 0xa0888215
	type1 := make([]byte, 32)
	copy(type1, "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(type1[8:], 1)
	binary.LittleEndian.PutUint32(type1[12:], flags)
	first := type1
	if spnego {
		first = wireSPNEGO(first, true)
	}
	r := c.sessionSetup(first)
	if r.status != wireMoreProcessing {
		return r
	}
	c.session = r.session
	if len(r.body) < 8 {
		c.t.Fatal("short SESSION_SETUP response")
	}
	security := c.wireSecurity(r, int(binary.LittleEndian.Uint16(r.body[4:])), int(binary.LittleEndian.Uint16(r.body[6:])))
	start := bytes.Index(security, []byte("NTLMSSP\x00"))
	if start < 0 || len(security)-start < 48 {
		c.t.Fatal("missing NTLM challenge")
	}
	challenge := security[start:]
	if binary.LittleEndian.Uint32(challenge[8:]) != 2 {
		c.t.Fatal("expected NTLM type 2")
	}
	infoLen := int(binary.LittleEndian.Uint16(challenge[40:]))
	infoOff := int(binary.LittleEndian.Uint32(challenge[44:]))
	if infoOff > len(challenge) || infoLen > len(challenge)-infoOff {
		c.t.Fatal("invalid NTLM target info")
	}
	info := challenge[infoOff : infoOff+infoLen]
	domain := ""
	stamp := uint64(time.Now().UnixNano()/100 + 116444736000000000)
	for av := info; len(av) >= 4; {
		kind, size := binary.LittleEndian.Uint16(av), int(binary.LittleEndian.Uint16(av[2:]))
		av = av[4:]
		if size > len(av) {
			c.t.Fatal("truncated NTLM target AV")
		}
		if kind == 2 {
			domain = wireString(av[:size])
		}
		if kind == 7 && size == 8 {
			stamp = binary.LittleEndian.Uint64(av)
		}
		av = av[size:]
	}
	type3 := make([]byte, 64)
	copy(type3, "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(type3[8:], 3)
	binary.LittleEndian.PutUint32(type3[60:], flags)
	var sessionKey []byte
	if user == "" {
		binary.LittleEndian.PutUint32(type3[60:], flags|0x800)
		type3 = wireSecBuf(type3, 12, []byte{0})
		for _, off := range []int{20, 28, 36, 44, 52} {
			type3 = wireSecBuf(type3, off, nil)
		}
	} else {
		ntHash := auth.MD4(wireUTF16(password))
		responseKey := wireMD5(ntHash[:], wireUTF16(strings.ToUpper(user)+domain))
		var nonce [8]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			c.t.Fatal(err)
		}
		blob := make([]byte, 28)
		blob[0], blob[1] = 1, 1
		binary.LittleEndian.PutUint64(blob[8:], stamp)
		copy(blob[16:], nonce[:])
		blob = append(blob, info...)
		blob = append(blob, 0, 0, 0, 0)
		proof := wireMD5(responseKey, append(append([]byte(nil), challenge[24:32]...), blob...))
		ntResponse := append(append([]byte(nil), proof...), blob...)
		lmResponse := wireMD5(responseKey, append(append([]byte(nil), challenge[24:32]...), nonce[:]...))
		lmResponse = append(lmResponse, nonce[:]...)
		sessionKey = wireMD5(responseKey, proof)
		type3 = wireSecBuf(type3, 12, lmResponse)
		type3 = wireSecBuf(type3, 20, ntResponse)
		type3 = wireSecBuf(type3, 28, wireUTF16(domain))
		type3 = wireSecBuf(type3, 36, wireUTF16(user))
		type3 = wireSecBuf(type3, 44, wireUTF16("OVNFS-TEST"))
		type3 = wireSecBuf(type3, 52, nil)
	}
	if spnego {
		type3 = wireSPNEGO(type3, false)
	}
	r = c.sessionSetup(type3)
	if r.status == wireSuccess {
		c.session = r.session
		if len(r.body) < 8 {
			c.t.Fatal("short final SESSION_SETUP")
		}
		guest := binary.LittleEndian.Uint16(r.body[2:])&3 != 0
		if user == "" && !guest {
			c.t.Fatal("anonymous login was not marked guest or null")
		}
		if !guest && len(sessionKey) != 0 {
			switch {
			case c.dialect >= 0x0311:
				c.key = auth.KDF(sessionKey, []byte("SMBSigningKey\x00"), c.lastRequestHash[:], 128)
			case c.dialect >= 0x0300:
				c.key = auth.KDF(sessionKey, []byte("SMB2AESCMAC\x00"), []byte("SmbSign\x00"), 128)
			default:
				c.key = sessionKey
			}
			c.verify(r)
		}
	}
	return r
}

// treeConnect mounts a share and updates the default tree only on success.
func (c *wireClient) treeConnect(share string) wireReply {
	name := wireUTF16(`\\127.0.0.1\` + share)
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b, 9)
	binary.LittleEndian.PutUint16(b[4:], 72)
	binary.LittleEndian.PutUint16(b[6:], uint16(len(name)))
	r := c.exchange(3, append(b, name...))
	if r.status == wireSuccess {
		c.tree = r.tree
	}
	return r
}

// wireCreateBody encodes a broadly shared read/write/delete open or directory open.
func wireCreateBody(name string, disposition, options uint32) []byte {
	b := make([]byte, 56)
	binary.LittleEndian.PutUint16(b, 57)
	binary.LittleEndian.PutUint32(b[4:], 2)
	binary.LittleEndian.PutUint32(b[24:], 0xc0010080)
	binary.LittleEndian.PutUint32(b[28:], 0x80)
	binary.LittleEndian.PutUint32(b[32:], 7)
	binary.LittleEndian.PutUint32(b[36:], disposition)
	binary.LittleEndian.PutUint32(b[40:], options)
	encoded := wireUTF16(name)
	binary.LittleEndian.PutUint16(b[44:], 120)
	binary.LittleEndian.PutUint16(b[46:], uint16(len(encoded)))
	return append(b, encoded...)
}

// create opens an object with explicit SMB create disposition and options.
func (c *wireClient) create(name string, disposition, options uint32) (wireID, wireReply) {
	c.t.Helper()
	r := c.exchange(5, wireCreateBody(name, disposition, options))
	var id wireID
	if r.status == wireSuccess {
		if len(r.body) < 88 {
			c.t.Fatal("short CREATE response")
		}
		copy(id[:], r.body[64:80])
	}
	return id, r
}

// close closes the wire handle and returns its observable protocol result.
func (c *wireClient) close(id wireID) wireReply {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint16(b, 24)
	copy(b[8:], id[:])
	return c.exchange(6, b)
}

// read requests bytes at an explicit offset, retaining EOF as a status.
func (c *wireClient) read(id wireID, offset uint64, length uint32) ([]byte, wireReply) {
	c.t.Helper()
	b := make([]byte, 49)
	binary.LittleEndian.PutUint16(b, 49)
	binary.LittleEndian.PutUint32(b[4:], length)
	binary.LittleEndian.PutUint64(b[8:], offset)
	copy(b[16:], id[:])
	r := c.exchange(8, b)
	if r.status != wireSuccess {
		return nil, r
	}
	if len(r.body) < 16 {
		c.t.Fatal("short READ response")
	}
	off, size := int(r.body[2]), int(binary.LittleEndian.Uint32(r.body[4:]))
	return c.wireSecurity(r, off, size), r
}

// write transfers data at an explicit offset and checks that no bytes were silently lost.
func (c *wireClient) write(id wireID, offset uint64, data []byte) wireReply {
	c.t.Helper()
	b := make([]byte, 48)
	binary.LittleEndian.PutUint16(b, 49)
	binary.LittleEndian.PutUint16(b[2:], 112)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(data)))
	binary.LittleEndian.PutUint64(b[8:], offset)
	copy(b[16:], id[:])
	r := c.exchange(9, append(b, data...))
	if r.status == wireSuccess && (len(r.body) < 8 || binary.LittleEndian.Uint32(r.body[4:]) != uint32(len(data))) {
		c.t.Fatal("short WRITE result")
	}
	return r
}

// queryDirectory requests a bounded page of FILE_NAMES_INFORMATION records.
func (c *wireClient) queryDirectory(id wireID, restart bool, size uint32) ([]string, wireReply) {
	c.t.Helper()
	b := make([]byte, 32)
	binary.LittleEndian.PutUint16(b, 33)
	b[2] = 12
	copy(b[8:], id[:])
	binary.LittleEndian.PutUint32(b[28:], size)
	if restart {
		b[3] = 1
		pattern := wireUTF16("*")
		binary.LittleEndian.PutUint16(b[24:], 96)
		binary.LittleEndian.PutUint16(b[26:], uint16(len(pattern)))
		b = append(b, pattern...)
	}
	r := c.exchange(14, b)
	if r.status != wireSuccess && r.status != 0x80000005 {
		return nil, r
	}
	if len(r.body) < 8 {
		c.t.Fatal("short QUERY_DIRECTORY response")
	}
	data := c.wireSecurity(r, int(binary.LittleEndian.Uint16(r.body[2:])), int(binary.LittleEndian.Uint32(r.body[4:])))
	var names []string
	for len(data) != 0 {
		if len(data) < 12 {
			c.t.Fatal("short directory record")
		}
		next, length := int(binary.LittleEndian.Uint32(data)), int(binary.LittleEndian.Uint32(data[8:]))
		if length > len(data)-12 || length%2 != 0 {
			c.t.Fatal("invalid directory name length")
		}
		names = append(names, wireString(data[12:12+length]))
		if next == 0 {
			break
		}
		if next < 12+length || next > len(data) || next%8 != 0 {
			c.t.Fatal("invalid directory continuation")
		}
		data = data[next:]
	}
	return names, r
}

// wireQueryBody asks for a file information class by handle.
func wireQueryBody(id wireID, class byte) []byte {
	b := make([]byte, 40)
	binary.LittleEndian.PutUint16(b, 41)
	b[2], b[3] = 1, class
	binary.LittleEndian.PutUint32(b[4:], 4096)
	copy(b[24:], id[:])
	return b
}

// queryInfo retrieves one file information payload, without parsing server-private types.
func (c *wireClient) queryInfo(id wireID, class byte) ([]byte, wireReply) {
	c.t.Helper()
	r := c.exchange(16, wireQueryBody(id, class))
	if r.status != wireSuccess {
		return nil, r
	}
	if len(r.body) < 8 {
		c.t.Fatal("short QUERY_INFO response")
	}
	return c.wireSecurity(r, int(binary.LittleEndian.Uint16(r.body[2:])), int(binary.LittleEndian.Uint32(r.body[4:]))), r
}

// setInfo sets a FILE information class on an existing handle.
func (c *wireClient) setInfo(id wireID, class byte, data []byte) wireReply {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint16(b, 33)
	b[2], b[3] = 1, class
	binary.LittleEndian.PutUint32(b[4:], uint32(len(data)))
	binary.LittleEndian.PutUint16(b[8:], 96)
	copy(b[16:], id[:])
	return c.exchange(17, append(b, data...))
}

// rename issues FILE_RENAME_INFORMATION with explicit replacement policy.
func (c *wireClient) rename(id wireID, target string, replace bool) wireReply {
	data := make([]byte, 20)
	if replace {
		data[0] = 1
	}
	name := wireUTF16(target)
	binary.LittleEndian.PutUint32(data[16:], uint32(len(name)))
	return c.setInfo(id, 10, append(data, name...))
}

// disposition marks an open for deletion on close.
func (c *wireClient) disposition(id wireID, remove bool) wireReply {
	var value byte
	if remove {
		value = 1
	}
	return c.setInfo(id, 13, []byte{value})
}

// ioctl sends a filesystem control and returns the server's output buffer.
func (c *wireClient) ioctl(id wireID, code uint32, input []byte, maxOut uint32) ([]byte, wireReply) {
	c.t.Helper()
	b := make([]byte, 56)
	binary.LittleEndian.PutUint16(b, 57)
	binary.LittleEndian.PutUint32(b[4:], code)
	copy(b[8:], id[:])
	if len(input) != 0 {
		binary.LittleEndian.PutUint32(b[24:], 120)
		binary.LittleEndian.PutUint32(b[28:], uint32(len(input)))
	}
	binary.LittleEndian.PutUint32(b[44:], maxOut)
	binary.LittleEndian.PutUint32(b[48:], 1)
	r := c.exchange(11, append(b, input...))
	if r.status != wireSuccess {
		return nil, r
	}
	if len(r.body) < 48 {
		c.t.Fatal("short IOCTL response")
	}
	off, count := int(binary.LittleEndian.Uint32(r.body[32:])), int(binary.LittleEndian.Uint32(r.body[36:]))
	if count == 0 {
		return nil, r
	}
	return c.wireSecurity(r, off, count), r
}

// lock acquires or releases one byte range using SMB2 LOCK flags.
func (c *wireClient) lock(id wireID, offset, length uint64, flags uint32) wireReply {
	b := make([]byte, 48)
	binary.LittleEndian.PutUint16(b, 48)
	binary.LittleEndian.PutUint16(b[2:], 1)
	copy(b[8:], id[:])
	binary.LittleEndian.PutUint64(b[24:], offset)
	binary.LittleEndian.PutUint64(b[32:], length)
	binary.LittleEndian.PutUint32(b[40:], flags)
	return c.exchange(10, b)
}

// compound creates, queries and closes one related handle in a single network exchange.
func (c *wireClient) compound(name string) []wireReply {
	c.t.Helper()
	var related wireID
	for i := range related {
		related[i] = 0xff
	}
	closeBody := make([]byte, 24)
	binary.LittleEndian.PutUint16(closeBody, 24)
	copy(closeBody[8:], related[:])
	parts := [][]byte{
		c.packet(5, wireCreateBody(name, 1, 0x40), 0, c.tree, c.session),
		c.packet(16, wireQueryBody(related, 6), wireRelated, ^uint32(0), ^uint64(0)),
		c.packet(6, closeBody, wireRelated, ^uint32(0), ^uint64(0)),
	}
	return c.compoundRequests(parts...)
}

// compoundRequests exchanges complete request headers and parses every reply.
func (c *wireClient) compoundRequests(parts ...[]byte) []wireReply {
	c.t.Helper()
	for i := range len(parts) - 1 {
		for len(parts[i])%8 != 0 {
			parts[i] = append(parts[i], 0)
		}
		binary.LittleEndian.PutUint32(parts[i][20:], uint32(len(parts[i])))
	}
	var request []byte
	for _, p := range parts {
		if len(c.key) != 0 {
			c.sign(p)
		}
		request = append(request, p...)
	}
	if err := c.writeFrame(request); err != nil {
		c.t.Fatal(err)
	}
	raw, err := c.readFrame()
	if err != nil {
		c.t.Fatal(err)
	}
	var replies []wireReply
	for len(raw) != 0 {
		if len(raw) < 64 {
			c.t.Fatal("short compound response")
		}
		next := int(binary.LittleEndian.Uint32(raw[20:]))
		n := len(raw)
		if next != 0 {
			if next < 64 || next > len(raw) || next%8 != 0 {
				c.t.Fatal("invalid compound response offset")
			}
			n = next
		}
		r, err := decodeWireReply(raw[:n])
		if err != nil {
			c.t.Fatal(err)
		}
		if len(c.key) != 0 {
			c.verify(r)
		}
		replies = append(replies, r)
		if next == 0 {
			break
		}
		raw = raw[next:]
	}
	if len(replies) != len(parts) {
		c.t.Fatalf("compound returned %d responses, want %d", len(replies), len(parts))
	}
	for i, part := range parts {
		command := binary.LittleEndian.Uint16(part[12:])
		if replies[i].command != command || binary.LittleEndian.Uint64(replies[i].raw[24:]) != binary.LittleEndian.Uint64(part[24:]) {
			c.t.Fatalf("compound response %d does not match request command %d", i, command)
		}
	}
	return replies
}
