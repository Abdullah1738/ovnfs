package mesh

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Key file names in a node's directory, as stencil-net names them.
const (
	keyFile  = "node_key"
	certFile = "node_cert.der"
	certName = "stencil-node"
)

// pkcs8Prefix is the PKCS#8 v1 DER of an Ed25519 key up to its 32-byte seed.
var pkcs8Prefix = []byte{0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x04, 0x22, 0x04, 0x20}

// pkcs8Seed is the Ed25519 algorithm identifier and the octet strings around
// the seed, found in v1 and v2 PKCS#8 alike.
var pkcs8Seed = pkcs8Prefix[5:]

// NodeKey is a node's private identity: its Ed25519 key and the
// self-signed certificate it presents in TLS.
type NodeKey struct {
	priv ed25519.PrivateKey
	id   NodeID
	cert tls.Certificate
}

// LoadOrCreateKey loads the node key in dir, creating dir (private) and a
// fresh key when there is none. The certificate beside it is regenerated
// over the same key when absent or not of that key, so the pin stays.
func LoadOrCreateKey(dir string) (*NodeKey, error) {
	keyPath := filepath.Join(dir, keyFile)
	certPath := filepath.Join(dir, certFile)
	var seed []byte
	der, err := os.ReadFile(keyPath)
	switch {
	case err == nil:
		if seed, err = seedOf(der); err != nil {
			return nil, fmt.Errorf("mesh: %s: %w", keyPath, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := writePrivate(keyPath, append(bytes.Clone(pkcs8Prefix), seed...)); err != nil {
			return nil, err
		}
		_ = os.Remove(certPath)
	default:
		return nil, err
	}
	k := newKey(seed)
	certDER, err := os.ReadFile(certPath)
	if err != nil || !certOf(certDER, k.id) {
		if certDER, err = selfSigned(k.priv); err != nil {
			return nil, err
		}
		if err := os.WriteFile(certPath, certDER, 0o644); err != nil {
			return nil, err
		}
	}
	k.cert = tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: k.priv}
	return k, nil
}

// ID is the node id of the key.
func (k *NodeKey) ID() NodeID { return k.id }

// Sign is the Ed25519 signature of msg by the key.
func (k *NodeKey) Sign(msg []byte) []byte { return ed25519.Sign(k.priv, msg) }

// Derive is a secret bound to this key and label (HMAC-SHA256 of the label
// under the key's seed): stable across restarts, distinct per label, and
// revealing nothing about the key. The node derives its QUIC stateless-reset
// key with it, so a restarted node ends the connections peers still hold.
func (k *NodeKey) Derive(label string) [32]byte {
	mac := hmac.New(sha256.New, k.priv.Seed())
	mac.Write([]byte(label))
	return [32]byte(mac.Sum(nil))
}

// newKey is the key of seed, without its certificate.
func newKey(seed []byte) *NodeKey {
	priv := ed25519.NewKeyFromSeed(seed)
	k := &NodeKey{priv: priv}
	copy(k.id[:], priv.Public().(ed25519.PublicKey))
	return k
}

// seedOf is the seed in a PKCS#8 Ed25519 key (v1 or v2).
func seedOf(der []byte) ([]byte, error) {
	if parsed, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if priv, ok := parsed.(ed25519.PrivateKey); ok {
			return priv.Seed(), nil
		}
		return nil, errors.New("not an Ed25519 key")
	}
	// v2 (with the public key attached), which crypto/x509 may refuse.
	if len(der) < 2 || der[0] != 0x30 {
		return nil, errors.New("not a PKCS#8 key")
	}
	at := bytes.Index(der, pkcs8Seed)
	if at < 0 || len(der) < at+len(pkcs8Seed)+ed25519.SeedSize {
		return nil, errors.New("not an Ed25519 key")
	}
	at += len(pkcs8Seed)
	return bytes.Clone(der[at : at+ed25519.SeedSize]), nil
}

// certOf reports whether der is a certificate of id's key.
func certOf(der []byte, id NodeID) bool {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return false
	}
	got, ok := spkiID(cert.RawSubjectPublicKeyInfo)
	return ok && got == id
}

// selfSigned is a self-signed certificate of priv naming stencil-node, valid
// 1975-01-01 to 4096-01-01 as rcgen makes it by default.
func selfSigned(priv ed25519.PrivateKey) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "rcgen self signed cert"},
		DNSNames:     []string{certName},
		NotBefore:    time.Date(1975, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(4096, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	return x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
}

// writePrivate writes b to a new file at path only its owner may read.
func writePrivate(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
