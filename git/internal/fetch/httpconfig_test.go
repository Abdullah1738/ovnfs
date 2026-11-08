package fetch

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newClientCert creates a self-signed client certificate, returning its
// PEM certificate and PKCS#8 key and a pool trusting it.
func newClientCert(t *testing.T) (certPEM, keyPEM []byte, pool *x509.CertPool) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "client"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), pool
}

func TestFetchHTTPSClientCertificate(t *testing.T) {
	s := newServer(t)
	certPEM, keyPEM, pool := newClientCert(t)
	hs := s.startHTTP(t, httpConfig{clientCAs: pool})
	s.g.vars["GIT_SSL_NO_VERIFY"] = "1"
	dir := t.TempDir()
	write := func(name string, data ...[]byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, bytes.Join(data, nil), 0o600); err != nil {
			t.Fatal(err)
		}
		return filepath.ToSlash(p)
	}
	cert, key, both := write("cert.pem", certPEM), write("key.pem", keyPEM), write("both.pem", certPEM, keyPEM)
	fetch := func(cfg string) error {
		t.Helper()
		_, gitDir, db := s.g.target()
		opts := s.g.options(hs.URL+"/src", gitDir, db, s.ids[4])
		opts.Depth = 1
		opts.Config = s.g.config(cfg)
		_, err := Fetch(context.Background(), opts)
		return err
	}
	if err := fetch(""); err == nil {
		t.Fatal("fetched without the required client certificate")
	}
	if err := fetch("[http]\nsslCert = " + cert + "\nsslKey = " + key + "\n"); err != nil {
		t.Fatalf("certificate and key: %v", err)
	}
	if err := fetch("[http]\nsslCert = " + both + "\n"); err != nil {
		t.Fatalf("key in the certificate's file: %v", err)
	}

	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("no openssl to encrypt a key with")
	}
	encrypted := filepath.Join(dir, "encrypted.pem")
	if out, err := exec.Command(openssl, "pkcs8", "-topk8", "-v2", "aes-256-cbc", "-in", key, "-out", encrypted, "-passout", "pass:secret").CombinedOutput(); err != nil {
		t.Fatalf("openssl: %v\n%s", err, out)
	}
	encryptedCfg := "[http]\nsslCert = " + cert + "\nsslKey = " + filepath.ToSlash(encrypted) + "\n"
	if err := fetch(encryptedCfg); err == nil || !strings.Contains(err.Error(), "http.sslCertPasswordProtected") {
		t.Fatalf("encrypted key without a way to ask: %v", err)
	}
	needScripts(t)
	logPath := credentialHelper(t, s.g, "secret")
	cfg := encryptedCfg + "\tsslCertPasswordProtected = true\n[credential]\n\thelper = fake\n"
	if err := fetch(cfg); err != nil {
		t.Fatalf("encrypted key: %v", err)
	}
	log, _ := os.ReadFile(logPath)
	if want := "args=get\nprotocol=cert\nhost=\npath=" + cert + "\nusername=\n"; !strings.HasPrefix(string(log), want) {
		t.Fatalf("helper log:\n%s\nwant prefix:\n%s", log, want)
	}
	if !strings.Contains(string(log), "args=store\nprotocol=cert\n") {
		t.Fatalf("passphrase not stored:\n%s", log)
	}
	credentialHelper(t, s.g, "wrong")
	if err := fetch(cfg); err == nil || !strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("wrong passphrase: %v", err)
	}
}

func TestFetchHTTPCookieFile(t *testing.T) {
	s := newServer(t)
	hs := s.startHTTP(t, httpConfig{cookie: "auth=token"})
	jar := filepath.Join(t.TempDir(), "cookies")
	host := strings.TrimPrefix(hs.URL, "http://")
	host = host[:strings.LastIndexByte(host, ':')]
	if err := os.WriteFile(jar, []byte("# Netscape HTTP Cookie File\n"+host+"\tFALSE\t/\tFALSE\t0\tauth\ttoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fetch := func(cfg string) error {
		t.Helper()
		_, gitDir, db := s.g.target()
		opts := s.g.options(hs.URL+"/src", gitDir, db, s.ids[4])
		opts.Config = s.g.config(cfg)
		_, err := Fetch(context.Background(), opts)
		return err
	}
	if err := fetch(""); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("without the cookie: %v", err)
	}
	if err := fetch("[http]\ncookieFile = " + filepath.ToSlash(jar) + "\n"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(jar); strings.Contains(string(data), "visit") {
		t.Fatalf("cookies saved without http.saveCookies:\n%s", data)
	}
	if err := fetch("[http]\ncookieFile = " + filepath.ToSlash(jar) + "\nsaveCookies = true\n"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(jar)
	for _, want := range []string{host + "\tFALSE\t/\tFALSE\t0\tauth\ttoken\n", host + "\tFALSE\t/\tFALSE\t0\tvisit\t1\n"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("saved cookies:\n%s\nlack %q", data, want)
		}
	}
}
