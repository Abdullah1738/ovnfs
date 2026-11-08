package fetch

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"os"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/config"
)

// clientCert is the TLS client certificate an HTTPS session presents:
// http.sslCert and http.sslKey (the key defaults to the certificate's file),
// their http.sslCertType and http.sslKeyType ("PEM" or "DER"), and
// http.sslCertPasswordProtected, each overridden by its GIT_SSL_*
// environment variable.
type clientCert struct {
	certFile, keyFile string
	certType, keyType string
	// protected allows asking for an encrypted key's passphrase.
	protected bool
}

// readClientCert reads the client certificate settings for the session's URL.
func readClientCert(get func(string) (config.Entry, bool, error), env environ) (clientCert, error) {
	var c clientCert
	var err error
	if c.certFile, err = pathSetting(get, env, "sslcert", "GIT_SSL_CERT"); err != nil {
		return c, err
	}
	if c.keyFile, err = pathSetting(get, env, "sslkey", "GIT_SSL_KEY"); err != nil {
		return c, err
	}
	if c.certType, err = setting(get, env, "sslcerttype", "GIT_SSL_CERT_TYPE"); err != nil {
		return c, err
	}
	if c.keyType, err = setting(get, env, "sslkeytype", "GIT_SSL_KEY_TYPE"); err != nil {
		return c, err
	}
	// Like GIT_SSL_NO_VERIFY, the variable counts by being set at all.
	if _, ok := env.lookup("GIT_SSL_CERT_PASSWORD_PROTECTED"); ok {
		c.protected = true
	} else if e, ok, err := get("sslcertpasswordprotected"); err != nil {
		return c, err
	} else if ok {
		if c.protected, err = e.Bool(); err != nil {
			return c, err
		}
	}
	return c, nil
}

// load reads the certificate chain and its private key. An encrypted key's
// passphrase comes from the credential helpers, else askpass (protocol=cert,
// path=the certificate file, as git asks), and is stored in the helpers once
// it decrypts the key; without http.sslCertPasswordProtected an encrypted
// key is an error, since nothing can prompt.
func (c clientCert) load(ctx context.Context, cfg *config.Config, env environ) (tls.Certificate, error) {
	data, err := os.ReadFile(c.certFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("fetch: http.sslCert: %w", err)
	}
	var chain [][]byte
	switch strings.ToUpper(c.certType) {
	case "", "PEM":
		for rest := data; ; {
			var b *pem.Block
			if b, rest = pem.Decode(rest); b == nil {
				break
			}
			if b.Type == "CERTIFICATE" {
				chain = append(chain, b.Bytes)
			}
		}
	case "DER":
		chain = [][]byte{data}
	default:
		return tls.Certificate{}, fmt.Errorf("fetch: http.sslCertType %q is not supported (PEM or DER)", c.certType)
	}
	if len(chain) == 0 {
		return tls.Certificate{}, fmt.Errorf("fetch: http.sslCert: no certificate in %s", c.certFile)
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("fetch: http.sslCert: %s: %w", c.certFile, err)
	}

	keyFile := c.keyFile
	if keyFile == "" {
		keyFile = c.certFile
	} else if data, err = os.ReadFile(keyFile); err != nil {
		return tls.Certificate{}, fmt.Errorf("fetch: http.sslKey: %w", err)
	}
	key, err := c.privateKey(ctx, cfg, env, keyFile, data)
	if err != nil {
		return tls.Certificate{}, err
	}
	pub, ok := key.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(leaf.PublicKey) {
		return tls.Certificate{}, fmt.Errorf("fetch: http.sslKey: the key in %s does not match the certificate in %s", keyFile, c.certFile)
	}
	return tls.Certificate{Certificate: chain, PrivateKey: key, Leaf: leaf}, nil
}

// privateKey decodes the key in data (read from file), decrypting it if need be.
func (c clientCert) privateKey(ctx context.Context, cfg *config.Config, env environ, file string, data []byte) (crypto.Signer, error) {
	var der []byte
	var decrypt func(pass string) ([]byte, error)
	switch strings.ToUpper(c.keyType) {
	case "", "PEM":
		var b *pem.Block
		for rest := data; ; {
			if b, rest = pem.Decode(rest); b == nil || strings.HasSuffix(b.Type, "PRIVATE KEY") {
				break
			}
		}
		switch {
		case b == nil:
			return nil, fmt.Errorf("fetch: http.sslKey: no private key in %s", file)
		case b.Type == "ENCRYPTED PRIVATE KEY":
			decrypt = func(pass string) ([]byte, error) { return decryptPKCS8(b.Bytes, pass) }
		case x509.IsEncryptedPEMBlock(b):
			// Legacy "Proc-Type: 4,ENCRYPTED" keys, as older OpenSSL wrote.
			decrypt = func(pass string) ([]byte, error) { return x509.DecryptPEMBlock(b, []byte(pass)) }
		default:
			der = b.Bytes
		}
	case "DER":
		der = data
		if _, err := parsePrivateKey(der); err != nil && isEncryptedPKCS8(der) {
			der, decrypt = nil, func(pass string) ([]byte, error) { return decryptPKCS8(data, pass) }
		}
	default:
		return nil, fmt.Errorf("fetch: http.sslKeyType %q is not supported (PEM or DER)", c.keyType)
	}
	if decrypt != nil {
		if !c.protected {
			return nil, fmt.Errorf("fetch: http.sslKey: %s is encrypted; set http.sslCertPasswordProtected to have its passphrase asked for", file)
		}
		var cred credential
		store, err := newCertCredentialStore(cfg, env, c.certFile, &cred)
		if err != nil {
			return nil, err
		}
		if err := store.fill(ctx, cfg, &cred); err != nil {
			return nil, err
		}
		if der, err = decrypt(cred.password); err != nil {
			return nil, fmt.Errorf("fetch: http.sslKey: cannot decrypt %s: %w", file, err)
		}
		key, err := parsePrivateKey(der)
		if err != nil {
			return nil, fmt.Errorf("fetch: http.sslKey: cannot decrypt %s: wrong passphrase", file)
		}
		store.approve(ctx, &cred)
		return key, nil
	}
	key, err := parsePrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("fetch: http.sslKey: %s: %w", file, err)
	}
	return key, nil
}

// parsePrivateKey decodes a PKCS#8, PKCS#1 (RSA) or SEC 1 (EC) private key.
func parsePrivateKey(der []byte) (crypto.Signer, error) {
	var key any
	var err error
	if key, err = x509.ParsePKCS8PrivateKey(der); err != nil {
		if key, err = x509.ParsePKCS1PrivateKey(der); err != nil {
			if key, err = x509.ParseECPrivateKey(der); err != nil {
				return nil, errors.New("unrecognised private key format")
			}
		}
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("unsupported private key type %T", key)
	}
	return signer, nil
}

// PKCS#8 encrypted private keys (RFC 5958) as OpenSSL writes them: PBES2
// (RFC 8018) with PBKDF2 and AES or triple DES in CBC mode.
var (
	oidPBES2  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
)

var pbkdf2Hashes = map[string]func() hash.Hash{
	"1.2.840.113549.2.7":  sha1.New,
	"1.2.840.113549.2.8":  sha256.New224,
	"1.2.840.113549.2.9":  sha256.New,
	"1.2.840.113549.2.10": sha512.New384,
	"1.2.840.113549.2.11": sha512.New,
}

var pbes2Ciphers = map[string]struct {
	keyLen int
	block  func(key []byte) (cipher.Block, error)
}{
	"2.16.840.1.101.3.4.1.2":  {16, aes.NewCipher},
	"2.16.840.1.101.3.4.1.22": {24, aes.NewCipher},
	"2.16.840.1.101.3.4.1.42": {32, aes.NewCipher},
	"1.2.840.113549.3.7":      {24, des.NewTripleDESCipher},
}

type encryptedPrivateKeyInfo struct {
	Algorithm pkix.AlgorithmIdentifier
	Data      []byte
}

type pbes2Params struct {
	KDF, Cipher pkix.AlgorithmIdentifier
}

type pbkdf2Params struct {
	Salt       []byte
	Iterations int
	KeyLength  int                      `asn1:"optional"`
	PRF        pkix.AlgorithmIdentifier `asn1:"optional"`
}

func isEncryptedPKCS8(der []byte) bool {
	var info encryptedPrivateKeyInfo
	rest, err := asn1.Unmarshal(der, &info)
	return err == nil && len(rest) == 0 && info.Algorithm.Algorithm.Equal(oidPBES2)
}

// decryptPKCS8 decrypts an EncryptedPrivateKeyInfo, returning the PKCS#8
// key it holds.
func decryptPKCS8(der []byte, pass string) ([]byte, error) {
	var info encryptedPrivateKeyInfo
	if _, err := asn1.Unmarshal(der, &info); err != nil {
		return nil, err
	}
	if !info.Algorithm.Algorithm.Equal(oidPBES2) {
		return nil, fmt.Errorf("unsupported key encryption %v", info.Algorithm.Algorithm)
	}
	var params pbes2Params
	if _, err := asn1.Unmarshal(info.Algorithm.Parameters.FullBytes, &params); err != nil {
		return nil, err
	}
	if !params.KDF.Algorithm.Equal(oidPBKDF2) {
		return nil, fmt.Errorf("unsupported key derivation %v", params.KDF.Algorithm)
	}
	var kdf pbkdf2Params
	if _, err := asn1.Unmarshal(params.KDF.Parameters.FullBytes, &kdf); err != nil {
		return nil, err
	}
	prf := sha1.New
	if len(kdf.PRF.Algorithm) > 0 {
		var ok bool
		if prf, ok = pbkdf2Hashes[kdf.PRF.Algorithm.String()]; !ok {
			return nil, fmt.Errorf("unsupported PBKDF2 hash %v", kdf.PRF.Algorithm)
		}
	}
	alg, ok := pbes2Ciphers[params.Cipher.Algorithm.String()]
	if !ok {
		return nil, fmt.Errorf("unsupported key cipher %v", params.Cipher.Algorithm)
	}
	var iv []byte
	if _, err := asn1.Unmarshal(params.Cipher.Parameters.FullBytes, &iv); err != nil {
		return nil, err
	}
	key, err := pbkdf2.Key(prf, pass, kdf.Salt, kdf.Iterations, alg.keyLen)
	if err != nil {
		return nil, err
	}
	block, err := alg.block(key)
	if err != nil {
		return nil, err
	}
	n := block.BlockSize()
	if len(iv) != n || len(info.Data) == 0 || len(info.Data)%n != 0 {
		return nil, errors.New("malformed encrypted key")
	}
	plain := make([]byte, len(info.Data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, info.Data)
	// PKCS#7 padding; a wrong passphrase almost always breaks it.
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > n || !bytes.Equal(plain[len(plain)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("wrong passphrase")
	}
	return plain[:len(plain)-pad], nil
}
