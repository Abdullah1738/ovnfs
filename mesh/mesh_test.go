package mesh

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// basePoint is the Ed25519 base point: a valid key.
func basePoint() NodeID {
	var id NodeID
	for i := range id {
		id[i] = 0x66
	}
	id[0] = 0x58
	return id
}

func TestNodeIDTextRoundTrip(t *testing.T) {
	hex := "58" + strings.Repeat("66", 31)
	b32 := "lbtgmztgmztgmztgmztgmztgmztgmztgmztgmztgmztgmztgmzta"
	for _, text := range []string{hex, strings.ToUpper(hex), b32, strings.ToUpper(b32)} {
		id, err := ParseNodeID(text)
		if err != nil || id != basePoint() {
			t.Fatalf("ParseNodeID(%q) = %v, %v", text, id, err)
		}
	}
	id := basePoint()
	if id.String() != hex || id.Base32() != b32 || id.Short() != "5866666666" {
		t.Fatalf("formats: %s %s %s", id, id.Base32(), id.Short())
	}
	if got := id.Fingerprint(); got != "SHA256:SuJVzkKh0nUW4/ZG5nXvT/6nOheWfuH1SVwBworu1GA" {
		t.Fatalf("fingerprint %s", got)
	}
	var back NodeID
	text, _ := id.MarshalText()
	if err := back.UnmarshalText(text); err != nil || back != id {
		t.Fatalf("text round trip: %v %v", back, err)
	}
}

func TestNodeIDRejectsNonKeysAndMalformedText(t *testing.T) {
	var off NodeID
	off[0] = 2
	for _, text := range []string{off.String(), off.Base32()} {
		if _, err := ParseNodeID(text); !errors.Is(err, ErrNotAKey) {
			t.Fatalf("ParseNodeID(%q) = %v, want ErrNotAKey", text, err)
		}
	}
	for _, text := range []string{
		"",
		strings.Repeat("58", 31),
		strings.Repeat("58", 32) + "0",
		"zz" + strings.Repeat("66", 31),
		strings.Repeat("a", 51) + "1",
		"lbtgmztgmztgmztgmztgmztgmztgmztgmztgmztgmztgmztgmztb", // spare bits set
	} {
		if _, err := ParseNodeID(text); !errors.Is(err, ErrMalformedID) {
			t.Fatalf("ParseNodeID(%q) = %v, want ErrMalformedID", text, err)
		}
	}
}

func TestKeyFilesPersistIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "node")
	k1, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	der, err := os.ReadFile(filepath.Join(dir, "node_key"))
	if err != nil {
		t.Fatal(err)
	}
	if len(der) != 48 || !bytes.HasPrefix(der, pkcs8Prefix) {
		t.Fatalf("key file is not PKCS#8 v1: % x", der)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, "node_key"))
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode %v", fi.Mode())
		}
		di, _ := os.Stat(dir)
		if di.Mode().Perm()&0o077 != 0 {
			t.Fatalf("key dir mode %v", di.Mode())
		}
	}
	if _, err := x509.ParsePKCS8PrivateKey(der); err != nil {
		t.Fatalf("key file does not parse: %v", err)
	}
	certPath := filepath.Join(dir, "node_cert.der")
	certDER, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "stencil-node" ||
		cert.NotBefore.Year() != 1975 || cert.NotAfter.Year() != 4096 {
		t.Fatalf("cert names %v validity %v..%v", cert.DNSNames, cert.NotBefore, cert.NotAfter)
	}
	if id, ok := spkiID(cert.RawSubjectPublicKeyInfo); !ok || id != k1.ID() {
		t.Fatal("cert is not over the node key")
	}

	k2, err := LoadOrCreateKey(dir)
	if err != nil || k2.ID() != k1.ID() {
		t.Fatalf("reload: %v %v", k2, err)
	}
	again, _ := os.ReadFile(certPath)
	if !bytes.Equal(again, certDER) {
		t.Fatal("an existing cert was rewritten")
	}
	// A lost certificate comes back over the same key: the pin stays.
	os.Remove(certPath)
	k3, err := LoadOrCreateKey(dir)
	if err != nil || k3.ID() != k1.ID() {
		t.Fatalf("after cert loss: %v", err)
	}
	if _, err := os.Stat(certPath); err != nil {
		t.Fatal("cert not regenerated")
	}
	msg := []byte("hello")
	if sig := k3.Sign(msg); len(sig) != 64 || !bytes.Equal(sig, k1.Sign(msg)) {
		t.Fatal("signatures differ across reloads")
	}
}

func TestWayParseAndPrint(t *testing.T) {
	good := map[string]Way{
		"udp:example.com:8377": {Host: "example.com", Port: 8377},
		"udp:10.0.0.1:1":       {Host: "10.0.0.1", Port: 1},
		"udp:[fd7a::1]:8377":   {Host: "fd7a::1", Port: 8377},
		"relay":                {Relay: true},
	}
	for text, want := range good {
		got, err := ParseWay(text)
		if err != nil || got != want {
			t.Fatalf("ParseWay(%q) = %+v, %v", text, got, err)
		}
		if got.String() != text {
			t.Fatalf("%+v prints %q, want %q", got, got.String(), text)
		}
	}
	if w, err := ParseWay("iroh:" + basePoint().String() + "@x"); err != nil || !w.Relay {
		t.Fatalf("iroh way: %v %v", w, err)
	}
	bad := map[string]error{
		"tcp:x:1":         ErrWayScheme,
		"udp:host":        ErrWayNoPort,
		"udp::1":          ErrWayNoHost,
		"udp:[]:1":        ErrWayNoHost,
		"udp:fd7a::1:1":   ErrWayHost,
		"udp:[host]:1":    ErrWayHost,
		"udp:a b:1":       ErrWayHost,
		"udp:host:0":      ErrWayPort,
		"udp:host:65536":  ErrWayPort,
		"udp:host:x":      ErrWayPort,
		"udp:[[::1]]:1":   ErrWayHost,
		"udp:host:":       ErrWayPort,
		"udp:host:-1":     ErrWayPort,
		"iroh:" + "zz":    ErrMalformedID,
		"udp:ho\x00st:80": ErrWayHost,
	}
	for text, want := range bad {
		if _, err := ParseWay(text); !errors.Is(err, want) {
			t.Fatalf("ParseWay(%q) = %v, want %v", text, err, want)
		}
	}
}

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), []byte{3, 0, 0, 0, 'a', 'b', 'c'}) {
		t.Fatalf("frame bytes % x", buf.Bytes())
	}
	p, err := ReadFrame(&buf, 3)
	if err != nil || string(p) != "abc" {
		t.Fatalf("ReadFrame = %q, %v", p, err)
	}
	// An oversized length is refused before its payload is read.
	r := &countingReader{r: bytes.NewReader([]byte{4, 0, 0, 0, 'a', 'b', 'c', 'd'})}
	if _, err := ReadFrame(r, 3); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if r.n != 4 {
		t.Fatalf("read %d bytes of an oversized frame", r.n)
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{2, 0, 0, 0, 'a'}), 8); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame: %v", err)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// handshake runs a TLS handshake between server and client over a pipe.
func handshake(t *testing.T, server, client *tls.Config) (*tls.Conn, *tls.Conn, error, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	b, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	a, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	a.SetDeadline(time.Now().Add(10 * time.Second))
	b.SetDeadline(time.Now().Add(10 * time.Second))
	s, c := tls.Server(a, server), tls.Client(b, client)
	done := make(chan error, 1)
	go func() {
		err := s.Handshake()
		if err != nil {
			a.Close()
		}
		done <- err
	}()
	cerr := c.Handshake()
	if cerr != nil {
		b.Close()
	}
	return s, c, <-done, cerr
}

func TestTLSPinsAndExporter(t *testing.T) {
	srvKey, err := LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cliKey, err := LoadOrCreateKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := srvKey.ID()
	s, c, serr, cerr := handshake(t, ServerTLS(srvKey, []string{"test"}), ClientTLS(cliKey, &want, "test"))
	if serr != nil || cerr != nil {
		t.Fatalf("pinned handshake: %v / %v", serr, cerr)
	}
	if id, ok := PeerID(c.ConnectionState()); !ok || id != srvKey.ID() {
		t.Fatal("client sees the wrong server id")
	}
	if id, ok := PeerID(s.ConnectionState()); !ok || id != cliKey.ID() {
		t.Fatal("server sees the wrong client id")
	}
	if c.ConnectionState().NegotiatedProtocol != "test" || c.ConnectionState().Version != tls.VersionTLS13 {
		t.Fatal("not TLS 1.3 with ALPN test")
	}
	if s.ConnectionState().ServerName != "stencil-node" {
		t.Fatalf("SNI %q", s.ConnectionState().ServerName)
	}
	e1, err1 := Exporter(s.ConnectionState(), "EXPORTER-test")
	e2, err2 := Exporter(c.ConnectionState(), "EXPORTER-test")
	if err1 != nil || err2 != nil || e1 != e2 || e1 == [32]byte{} {
		t.Fatalf("exporters differ: %x %x %v %v", e1, e2, err1, err2)
	}

	// Any key when nothing is pinned.
	if _, _, serr, cerr := handshake(t, ServerTLS(srvKey, []string{"test"}), ClientTLS(cliKey, nil, "test")); serr != nil || cerr != nil {
		t.Fatalf("unpinned handshake: %v / %v", serr, cerr)
	}

	// Another key than the pinned one fails with the key seen.
	other := basePoint()
	_, _, _, cerr = handshake(t, ServerTLS(srvKey, []string{"test"}), ClientTLS(cliKey, &other, "test"))
	var pin *PinError
	if !errors.As(cerr, &pin) || pin.Seen != srvKey.ID() || pin.Expected != other {
		t.Fatalf("wrong pin: %v", cerr)
	}

	// A client without a certificate is allowed (requested, not required).
	bare := ClientTLS(cliKey, &want, "test")
	bare.Certificates = nil
	s, _, serr, cerr = handshake(t, ServerTLS(srvKey, []string{"test"}), bare)
	if serr != nil || cerr != nil {
		t.Fatalf("certless client: %v / %v", serr, cerr)
	}
	if _, ok := PeerID(s.ConnectionState()); ok {
		t.Fatal("certless client has an id")
	}
}
