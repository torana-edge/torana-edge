package mitm

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// CA is a locally-generated certificate authority that mints per-host leaf
// certificates on demand. The private key lives only in the configured dir.
type CA struct {
	cert  *x509.Certificate
	key   *ecdsa.PrivateKey
	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

// LoadOrCreateCA loads the CA from dir, generating a new one if absent.
func LoadOrCreateCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pathInfo, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("mitm CA directory must not be a symlink: %q", dir)
	}
	// MkdirAll does not tighten an existing directory (including the common
	// freshly-created 0755 test/config directory). CADir is dedicated security
	// material, so normalize it instead of making the operator repair the
	// default umask by hand.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("secure MITM CA directory: %w", err)
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("mitm CA path %q is not a directory", dir)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("mitm CA directory %q permissions are %04o after chmod; want 0700 or stricter", dir, info.Mode().Perm())
	}
	certPath := filepath.Join(dir, "ca-cert.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	certInfo, certErr := os.Lstat(certPath)
	keyInfo, keyErr := os.Lstat(keyPath)
	certMissing := os.IsNotExist(certErr)
	keyMissing := os.IsNotExist(keyErr)
	if certErr != nil && !certMissing {
		return nil, fmt.Errorf("stat MITM CA certificate: %w", certErr)
	}
	if keyErr != nil && !keyMissing {
		return nil, fmt.Errorf("stat MITM CA private key: %w", keyErr)
	}
	if certMissing != keyMissing {
		return nil, fmt.Errorf("incomplete MITM CA in %q: certificate and private key must both exist or both be absent", dir)
	}
	if !certMissing {
		if certInfo.Mode()&os.ModeSymlink != 0 || keyInfo.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("MITM CA certificate and private key must not be symlinks")
		}
		if !certInfo.Mode().IsRegular() || !keyInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("MITM CA certificate and private key must be regular files")
		}
		if runtime.GOOS != "windows" && keyInfo.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("MITM CA private key permissions are %04o; want 0600 or stricter", keyInfo.Mode().Perm())
		}
		cert, key, err := loadCA(certPath, keyPath, time.Now())
		if err != nil {
			return nil, err
		}
		return &CA{cert: cert, key: key, cache: map[string]*tls.Certificate{}}, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Torana MITM CA (dev)"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	if err := writePEMAtomic(certPath, 0o644, "CERTIFICATE", der); err != nil {
		return nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writePEMAtomic(keyPath, 0o600, "EC PRIVATE KEY", kder); err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key, cache: map[string]*tls.Certificate{}}, nil
}

// LeafFor returns a leaf certificate for name, minting and caching it if new.
func (c *CA) LeafFor(name string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cert, ok := c.cache[name]; ok {
		return cert, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key}
	c.cache[name] = cert
	return cert, nil
}

// WriteBundle writes a CA bundle (system roots + our CA) to bundle.pem so the
// client can validate both our MITM leaves and real upstream certs (for
// tunneled hosts). Returns the bundle path.
func (c *CA) WriteBundle(dir string) (string, error) {
	var sys []byte
	for _, p := range []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/cert.pem"} {
		if b, err := os.ReadFile(p); err == nil {
			sys = b
			break
		}
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
	out := append(append(append([]byte{}, sys...), '\n'), caPEM...)
	path := filepath.Join(dir, "bundle.pem")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func loadCA(certPath, keyPath string, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read MITM CA certificate: %w", err)
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read MITM CA private key: %w", err)
	}
	cblock, crest := pem.Decode(cb)
	kblock, krest := pem.Decode(kb)
	if cblock == nil || cblock.Type != "CERTIFICATE" || len(bytes.TrimSpace(crest)) != 0 {
		return nil, nil, fmt.Errorf("parse MITM CA certificate: expected exactly one CERTIFICATE PEM block")
	}
	if kblock == nil || kblock.Type != "EC PRIVATE KEY" || len(bytes.TrimSpace(krest)) != 0 {
		return nil, nil, fmt.Errorf("parse MITM CA private key: expected exactly one EC PRIVATE KEY PEM block")
	}
	cert, err := x509.ParseCertificate(cblock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse MITM CA certificate: %w", err)
	}
	key, err := x509.ParseECPrivateKey(kblock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse MITM CA private key: %w", err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, nil, fmt.Errorf("MITM CA certificate and private key do not match")
	}
	if !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, nil, fmt.Errorf("MITM CA certificate is not valid for certificate signing")
	}
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return nil, nil, fmt.Errorf("MITM CA certificate is not currently valid")
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		return nil, nil, fmt.Errorf("MITM CA certificate is not self-signed: %w", err)
	}
	return cert, key, nil
}

func writePEMAtomic(path string, mode os.FileMode, blockType string, der []byte) error {
	return writeFileAtomic(path, mode, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}))
}

func writeFileAtomic(path string, mode os.FileMode, contents []byte) (retErr error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".torana-ca-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(contents); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}
