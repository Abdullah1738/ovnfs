// Package mesh holds the wire-level building blocks shared by the ovnfs mesh
// transport: node identities and key files, TLS configurations that pin
// peers by key, ways to reach a node, and length-prefixed frames. Every
// format here is byte-compatible with stencil-net.
package mesh

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
)

// DefaultPort is the UDP port a mesh node listens on unless told otherwise.
const DefaultPort = 8377

// Application close codes a node sends when it ends a connection.
const (
	Denied     = 1 // refused by admission, rate-limited, no node certificate, or too slow
	Restarting = 3 // the node shuts down; reconnect later
	NotServing = 5 // the protocol is declared but nothing listens on it now
	Busy       = 6 // the listener's backlog is full
)

// ed25519SPKI is the DER SubjectPublicKeyInfo prefix of an Ed25519 key; the
// 32 key bytes follow it.
var ed25519SPKI = []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00}

// NodeID is a node's identity: its Ed25519 public key.
type NodeID [32]byte

// ErrMalformedID reports node id text that is neither 64 hex nor 52 base32 characters.
var ErrMalformedID = errors.New("mesh: malformed node id")

// ErrNotAKey reports node id bytes that are no point on the Ed25519 curve.
var ErrNotAKey = errors.New("mesh: node id is not an Ed25519 public key")

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// ParseNodeID parses 64 hex digits (either case) or 52 unpadded RFC 4648
// base32 characters (either case, zero spare bits) naming a valid key.
func ParseNodeID(s string) (NodeID, error) {
	var id NodeID
	switch len(s) {
	case 64:
		if _, err := hex.Decode(id[:], []byte(s)); err != nil {
			return NodeID{}, ErrMalformedID
		}
	case 52:
		up := strings.ToUpper(s)
		n, err := b32.Decode(id[:], []byte(up))
		if err != nil || n != 32 || b32.EncodeToString(id[:]) != up {
			return NodeID{}, ErrMalformedID
		}
	default:
		return NodeID{}, ErrMalformedID
	}
	if !onCurve(id) {
		return NodeID{}, ErrNotAKey
	}
	return id, nil
}

// String is the id as 64 lowercase hex digits.
func (id NodeID) String() string { return hex.EncodeToString(id[:]) }

// Base32 is the id as 52 lowercase unpadded base32 characters.
func (id NodeID) Base32() string { return strings.ToLower(b32.EncodeToString(id[:])) }

// Short is the first five bytes in hex, for logs.
func (id NodeID) Short() string { return hex.EncodeToString(id[:5]) }

// Fingerprint is the pin peers keep for the id: "SHA256:" and the unpadded
// base64 SHA-256 of its SubjectPublicKeyInfo.
func (id NodeID) Fingerprint() string {
	h := sha256.New()
	h.Write(ed25519SPKI)
	h.Write(id[:])
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(h.Sum(nil))
}

// MarshalText encodes the id as 64 lowercase hex digits.
func (id NodeID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText parses the id as ParseNodeID does.
func (id *NodeID) UnmarshalText(text []byte) error {
	parsed, err := ParseNodeID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// Curve constants of edwards25519: the field prime and d.
var (
	fieldP = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	curveD = func() *big.Int {
		// d = -121665 / 121666 mod p
		num := new(big.Int).Neg(big.NewInt(121665))
		den := new(big.Int).ModInverse(big.NewInt(121666), fieldP)
		d := num.Mul(num, den)
		return d.Mod(d, fieldP)
	}()
)

// onCurve reports whether id decompresses to a point on edwards25519, with
// the lenient decoding of ed25519-dalek (non-canonical y accepted): x² =
// (y²-1)/(d·y²+1) must have a square root mod p.
func onCurve(id NodeID) bool {
	le := id
	le[31] &= 0x7f
	for i, j := 0, 31; i < j; i, j = i+1, j-1 {
		le[i], le[j] = le[j], le[i]
	}
	y := new(big.Int).SetBytes(le[:])
	y.Mod(y, fieldP)
	yy := new(big.Int).Mul(y, y)
	yy.Mod(yy, fieldP)
	u := new(big.Int).Sub(yy, big.NewInt(1))
	u.Mod(u, fieldP)
	v := new(big.Int).Mul(curveD, yy)
	v.Add(v, big.NewInt(1))
	v.Mod(v, fieldP)
	if v.Sign() == 0 {
		return false
	}
	x2 := new(big.Int).ModInverse(v, fieldP)
	x2.Mul(x2, u)
	x2.Mod(x2, fieldP)
	if x2.Sign() == 0 {
		return true
	}
	return new(big.Int).ModSqrt(x2, fieldP) != nil
}
