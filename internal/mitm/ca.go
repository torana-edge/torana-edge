package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	certPath := filepath.Join(dir, "ca-cert.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	if cb, err := os.ReadFile(certPath); err == nil {
		if kb, err := os.ReadFile(keyPath); err == nil {
			cblock, _ := pem.Decode(cb)
			kblock, _ := pem.Decode(kb)
			if cblock != nil && kblock != nil {
				cert, e1 := x509.ParseCertificate(cblock.Bytes)
				key, e2 := x509.ParseECPrivateKey(kblock.Bytes)
				if e1 == nil && e2 == nil {
					return &CA{cert: cert, key: key, cache: map[string]*tls.Certificate{}}, nil
				}
			}
		}
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
	if err := writePEM(certPath, 0o644, "CERTIFICATE", der); err != nil {
		return nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writePEM(keyPath, 0o600, "EC PRIVATE KEY", kder); err != nil {
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

func writePEM(path string, mode os.FileMode, blockType string, der []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: der})
}
