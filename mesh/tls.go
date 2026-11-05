package mesh

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

// ServerName is the SNI a node dial offers; the server ignores it.
const ServerName = certName

// ErrNotEd25519 reports a peer certificate whose key is not Ed25519.
var ErrNotEd25519 = errors.New("mesh: peer certificate key is not Ed25519")

// PinError reports a peer that proved a key other than the pinned one.
type PinError struct {
	Expected NodeID
	Seen     NodeID
}

// Error names both keys.
func (e *PinError) Error() string {
	return fmt.Sprintf("mesh: the key there is %s, not node %s's", e.Seen.Fingerprint(), e.Expected)
}

// ServerTLS is the TLS 1.3 configuration a node serves alpns with: it
// requests (does not require) a client certificate and accepts any
// Ed25519 one, checking neither names nor validity.
func ServerTLS(k *NodeKey, alpns []string) *tls.Config {
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		MaxVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{k.cert},
		NextProtos:             append([]string(nil), alpns...),
		ClientAuth:             tls.RequestClientCert,
		SessionTicketsDisabled: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return nil
			}
			_, err := certID(raw[0])
			return err
		},
	}
}

// ClientTLS is the mutual-TLS 1.3 configuration a node dials alpn with: it
// presents k's certificate and accepts only expect's key (nil: any Ed25519
// key), ignoring names and validity. A pin mismatch fails with *PinError.
func ClientTLS(k *NodeKey, expect *NodeID, alpn string) *tls.Config {
	var pin *NodeID
	if expect != nil {
		e := *expect
		pin = &e
	}
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		MaxVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{k.cert},
		NextProtos:             []string{alpn},
		ServerName:             ServerName,
		InsecureSkipVerify:     true, // pin-only verification below
		SessionTicketsDisabled: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("mesh: peer presented no certificate")
			}
			id, err := certID(raw[0])
			if err != nil {
				return err
			}
			if pin != nil && id != *pin {
				return &PinError{Expected: *pin, Seen: id}
			}
			return nil
		},
	}
}

// PeerID is the node id of the peer's first certificate, when its key is
// an Ed25519 key.
func PeerID(cs tls.ConnectionState) (NodeID, bool) {
	if len(cs.PeerCertificates) == 0 {
		return NodeID{}, false
	}
	return spkiID(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
}

// Exporter is 32 bytes of keying material exported under label with an
// empty context, as both ends of the connection compute it.
func Exporter(cs tls.ConnectionState, label string) ([32]byte, error) {
	var out [32]byte
	b, err := cs.ExportKeyingMaterial(label, []byte{}, len(out))
	if err != nil {
		return out, err
	}
	copy(out[:], b)
	return out, nil
}

// certID is the node id of the DER certificate der.
func certID(der []byte) (NodeID, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return NodeID{}, err
	}
	id, ok := spkiID(cert.RawSubjectPublicKeyInfo)
	if !ok {
		return NodeID{}, ErrNotEd25519
	}
	return id, nil
}

// spkiID is the key in an Ed25519 SubjectPublicKeyInfo.
func spkiID(spki []byte) (NodeID, bool) {
	var id NodeID
	if len(spki) != len(ed25519SPKI)+len(id) || !bytes.HasPrefix(spki, ed25519SPKI) {
		return id, false
	}
	copy(id[:], spki[len(ed25519SPKI):])
	return id, true
}
