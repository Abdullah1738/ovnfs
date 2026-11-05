package relay

// The node relay's wire format (`GET /node`, subprotocols stencil-relay.v1
// and stencil-relay.v2), ported from stencil's relay service
// (apps/relay/node_frames.go) and stencil-net's client
// (crates/net/src/relay/proto.rs). testdata/node-frames.txt holds the
// golden vectors all three implementations are tested against.
//
// Each binary WebSocket message holds frames back to back: a type byte, the
// body's length as a little-endian u16, the body. Integers are little-endian;
// addresses are a family byte (4 or 6), the address, the port.
//
// Readers skip frame types they do not know and ignore whatever a body holds
// past the fields its type needs. A body too short for its type, an unknown
// address family, or a header or body running past the message is an error,
// and nothing after it is read.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"time"

	"github.com/stencil-hq/ovnfs/mesh"
)

const (
	protocolV1  = "stencil-relay.v1"
	protocolV2  = "stencil-relay.v2"
	nodePath    = "/node"
	maxMessage  = 1 << 20    // largest WebSocket message either way
	maxDatagram = 65527      // largest datagram a send or recv carries
	maxSendTo   = 65535 - 32 // largest datagram a send-to carries (u16 body)
	frameHeader = 3          // type byte + u16 body length
	// challengeDomain prefixes the nonce a node signs to prove its id.
	challengeDomain = "stencil-relay v1 challenge"
)

// Frame type bytes: node → relay below 0x80, relay → node above.
const (
	kindAuth      byte = 0x01 // node, sig
	kindConnect   byte = 0x02 // peer
	kindSend      byte = 0x03 // route, datagram
	kindPing      byte = 0x04 // n
	kindRelease   byte = 0x05 // route
	kindSendTo    byte = 0x06 // peer, datagram (v2 links only)
	kindChallenge byte = 0x81 // nonce
	kindWelcome   byte = 0x82 // observed
	kindRoute     byte = 0x83 // peer, route, observed
	kindNoPeer    byte = 0x84 // peer
	kindRecv      byte = 0x85 // route, datagram
	kindPong      byte = 0x86 // n
	kindGone      byte = 0x87 // route
	kindClosing   byte = 0x88 // reason, retry_ms
)

// closeReason says why the relay ends a link (the closing frame).
type closeReason byte

const (
	reasonReplaced   closeReason = 1 // a newer link registered the node
	reasonRestarting closeReason = 2 // the relay is shutting down
	reasonLimited    closeReason = 3 // over the network's usage limits
	reasonProtocol   closeReason = 4 // the node broke the protocol or failed its proof
	reasonRefused    closeReason = 5 // the relay does not serve the node
)

// String names the reason as the relay's logs do.
func (r closeReason) String() string {
	switch r {
	case reasonReplaced:
		return "replaced"
	case reasonRestarting:
		return "restarting"
	case reasonLimited:
		return "limited"
	case reasonProtocol:
		return "protocol"
	case reasonRefused:
		return "refused"
	}
	return fmt.Sprintf("reason %d", byte(r))
}

// frame is one frame of any kind; kind says which fields are meaningful.
// A decoded datagram aliases the message it was read from.
type frame struct {
	kind     byte
	peer     mesh.NodeID // auth's node; connect's, send-to's, route's and no-peer's peer
	sig      [64]byte
	nonce    [32]byte
	route    uint32
	datagram []byte
	n        uint64 // ping, pong
	observed netip.AddrPort
	reason   closeReason
	retry    time.Duration // closing's retry_ms
}

// errTruncated: a frame header or body runs past the end of its message.
var errTruncated = errors.New("relay frame runs past its message")

// bodyError: a body too short for its type byte, or malformed.
type bodyError byte

func (e bodyError) Error() string {
	return fmt.Sprintf("malformed body for relay frame type %02x", byte(e))
}

// addrLen is the bytes addr takes on the wire. The zero AddrPort goes as
// 0.0.0.0:0.
func addrLen(addr netip.AddrPort) int {
	if addr.Addr().Unmap().Is6() {
		return 1 + 16 + 2
	}
	return 1 + 4 + 2
}

func appendAddr(b []byte, addr netip.AddrPort) []byte {
	ip := addr.Addr().Unmap()
	switch {
	case ip.Is6():
		a := ip.As16()
		b = append(b, 6)
		b = append(b, a[:]...)
	case ip.Is4():
		a := ip.As4()
		b = append(b, 4)
		b = append(b, a[:]...)
	default:
		b = append(b, 4, 0, 0, 0, 0)
	}
	return binary.LittleEndian.AppendUint16(b, addr.Port())
}

func readAddr(b []byte) (netip.AddrPort, bool) {
	if len(b) == 0 {
		return netip.AddrPort{}, false
	}
	var ip netip.Addr
	switch b[0] {
	case 4:
		if len(b) < 1+4+2 {
			return netip.AddrPort{}, false
		}
		ip = netip.AddrFrom4([4]byte(b[1:5]))
		b = b[5:]
	case 6:
		if len(b) < 1+16+2 {
			return netip.AddrPort{}, false
		}
		ip = netip.AddrFrom16([16]byte(b[1:17]))
		b = b[17:]
	default:
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(ip, binary.LittleEndian.Uint16(b)), true
}

// bodyLen is the length of f's body as appendFrame writes it.
func bodyLen(f *frame) int {
	switch f.kind {
	case kindAuth:
		return 32 + 64
	case kindConnect, kindNoPeer, kindChallenge:
		return 32
	case kindSend, kindRecv:
		return 4 + len(f.datagram)
	case kindSendTo:
		return 32 + len(f.datagram)
	case kindPing, kindPong:
		return 8
	case kindRelease, kindGone:
		return 4
	case kindWelcome:
		return addrLen(f.observed)
	case kindRoute:
		return 32 + 4 + addrLen(f.observed)
	case kindClosing:
		return 1 + 4
	}
	panic(fmt.Sprintf("relay frame type %02x has no encoding", f.kind))
}

// frameLen is the bytes f takes in a message.
func frameLen(f *frame) int { return frameHeader + bodyLen(f) }

// appendFrame appends f's encoding to b. Datagrams must not exceed
// maxDatagram (maxSendTo for send-to), so every body fits its u16 length.
func appendFrame(b []byte, f *frame) []byte {
	b = append(b, f.kind)
	b = binary.LittleEndian.AppendUint16(b, uint16(bodyLen(f)))
	switch f.kind {
	case kindAuth:
		b = append(b, f.peer[:]...)
		b = append(b, f.sig[:]...)
	case kindConnect, kindNoPeer:
		b = append(b, f.peer[:]...)
	case kindChallenge:
		b = append(b, f.nonce[:]...)
	case kindSend, kindRecv:
		b = binary.LittleEndian.AppendUint32(b, f.route)
		b = append(b, f.datagram...)
	case kindSendTo:
		b = append(b, f.peer[:]...)
		b = append(b, f.datagram...)
	case kindPing, kindPong:
		b = binary.LittleEndian.AppendUint64(b, f.n)
	case kindRelease, kindGone:
		b = binary.LittleEndian.AppendUint32(b, f.route)
	case kindWelcome:
		b = appendAddr(b, f.observed)
	case kindRoute:
		b = append(b, f.peer[:]...)
		b = binary.LittleEndian.AppendUint32(b, f.route)
		b = appendAddr(b, f.observed)
	case kindClosing:
		b = append(b, byte(f.reason))
		b = binary.LittleEndian.AppendUint32(b, uint32(min(f.retry.Milliseconds(), math.MaxUint32)))
	}
	return b
}

// frameReader reads the frames of one message in order. Once it meets an
// error it stops; err then says why.
type frameReader struct {
	rest []byte
	err  error
}

// next returns the next frame the reader knows, false at the end of the
// message or at an error.
func (r *frameReader) next() (frame, bool) {
	for r.err == nil && len(r.rest) > 0 {
		if len(r.rest) < frameHeader {
			r.err = errTruncated
			return frame{}, false
		}
		kind, n := r.rest[0], int(binary.LittleEndian.Uint16(r.rest[1:frameHeader]))
		if len(r.rest)-frameHeader < n {
			r.err = errTruncated
			return frame{}, false
		}
		body := r.rest[frameHeader : frameHeader+n]
		r.rest = r.rest[frameHeader+n:]
		f, known, ok := decodeBody(kind, body)
		if !ok {
			r.err = bodyError(kind)
			return frame{}, false
		}
		if known {
			return f, true
		}
	}
	return frame{}, false
}

// decodeBody reads a body of type kind: known is false for a type this
// version does not know, ok false for a malformed body.
func decodeBody(kind byte, body []byte) (f frame, known, ok bool) {
	f.kind = kind
	switch kind {
	case kindAuth:
		if len(body) < 32+64 {
			return f, true, false
		}
		f.peer = mesh.NodeID(body[:32])
		f.sig = [64]byte(body[32:96])
	case kindConnect, kindNoPeer:
		if len(body) < 32 {
			return f, true, false
		}
		f.peer = mesh.NodeID(body[:32])
	case kindChallenge:
		if len(body) < 32 {
			return f, true, false
		}
		f.nonce = [32]byte(body[:32])
	case kindSend, kindRecv:
		if len(body) < 4 {
			return f, true, false
		}
		f.route = binary.LittleEndian.Uint32(body)
		f.datagram = body[4:]
	case kindSendTo:
		if len(body) < 32 {
			return f, true, false
		}
		f.peer = mesh.NodeID(body[:32])
		f.datagram = body[32:]
	case kindPing, kindPong:
		if len(body) < 8 {
			return f, true, false
		}
		f.n = binary.LittleEndian.Uint64(body)
	case kindRelease, kindGone:
		if len(body) < 4 {
			return f, true, false
		}
		f.route = binary.LittleEndian.Uint32(body)
	case kindWelcome:
		if f.observed, ok = readAddr(body); !ok {
			return f, true, false
		}
	case kindRoute:
		if len(body) < 32+4 {
			return f, true, false
		}
		f.peer = mesh.NodeID(body[:32])
		f.route = binary.LittleEndian.Uint32(body[32:])
		if f.observed, ok = readAddr(body[36:]); !ok {
			return f, true, false
		}
	case kindClosing:
		if len(body) < 1+4 {
			return f, true, false
		}
		f.reason = closeReason(body[0])
		f.retry = time.Duration(binary.LittleEndian.Uint32(body[1:])) * time.Millisecond
	default:
		return f, false, true
	}
	return f, true, true
}

// challengeMessage is what a node signs to answer nonce:
// "stencil-relay v1 challenge" || nonce.
func challengeMessage(nonce [32]byte) []byte {
	return append([]byte(challengeDomain), nonce[:]...)
}
