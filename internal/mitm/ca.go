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
	"sync"
	"time"

	"github.com/torana-edge/torana-edge/internal/fileperm"
)

// leafLifetime bounds a minted leaf. Short on purpose: these are minted by a
// local interception CA, and a short life limits the damage if one leaks.
const leafLifetime = 24 * time.Hour

// leafRenewBefore re-mints a cached leaf this long before it expires, so a
// handshake never hands the client a certificate about to lapse mid-connection.
const leafRenewBefore = time.Hour

// CA is a locally-generated certificate authority that mints per-host leaf
// certificates on demand. The private key lives only in the configured dir.
type CA struct {
	cert  *x509.Certificate
	key   *ecdsa.PrivateKey
	mu    sync.Mutex
	cache map[string]cachedLeaf
	// now is injectable so the expiry path can be tested without sleeping for
	// a day. Nil means time.Now.
	now func() time.Time
}

// cachedLeaf pairs a minted certificate with the expiry the cache must respect.
// tls.Certificate.Leaf is not guaranteed to be populated, so the deadline is
// recorded at mint time rather than re-parsed on every handshake.
type cachedLeaf struct {
	cert     *tls.Certificate
	notAfter time.Time
}

func (c *CA) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
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
	if err := fileperm.RestrictDir(dir); err != nil {
		return nil, fmt.Errorf("secure MITM CA directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("mitm CA path %q is not a directory", dir)
	}
	if err := fileperm.Verify(dir, info); err != nil {
		return nil, fmt.Errorf("mitm CA directory: %w", err)
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
		// This key mints certificates for every intercepted host, so it is
		// owner-only on every platform. The runtime.GOOS guard that stood here
		// SKIPPED the check on Windows rather than expressing it there, which
		// is the same hole the audit log and the credential key had.
		if err := fileperm.Verify(keyPath, keyInfo); err != nil {
			return nil, fmt.Errorf("MITM CA private key: %w", err)
		}
		cert, key, err := loadCA(certPath, keyPath, time.Now())
		if err != nil {
			return nil, err
		}
		return &CA{cert: cert, key: key, cache: map[string]cachedLeaf{}}, nil
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
	if err := writePEMAtomic(certPath, false, "CERTIFICATE", der); err != nil {
		return nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writePEMAtomic(keyPath, true, "EC PRIVATE KEY", kder); err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key, cache: map[string]cachedLeaf{}}, nil
}

// LeafFor returns a leaf certificate for name, minting one when the cache has
// none or the cached one is about to expire.
//
// The expiry check is the point. Leaves live 24h and the cache never evicted,
// so a proxy left running overnight served an EXPIRED certificate to every
// subsequent handshake and the ingress simply stopped working — with a TLS
// error that points at the client, not here.
func (c *CA) LeafFor(name string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if cached, ok := c.cache[name]; ok && now.Add(leafRenewBefore).Before(cached.notAfter) {
		return cached.cert, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	notAfter := now.Add(leafLifetime)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		// A random 128-bit serial. UnixNano() collides for two leaves minted
		// in the same nanosecond and leaks mint time; neither is wanted in a
		// certificate a client pins.
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key}
	c.cache[name] = cachedLeaf{cert: cert, notAfter: notAfter}
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
	// Without system roots the bundle contains only this CA, so the client can
	// validate intercepted hosts but not the REAL certificates of every
	// tunneled one — which is precisely what the bundle exists to carry. That
	// failed as a confusing per-host TLS error much later; say it here.
	if len(sys) == 0 {
		return "", fmt.Errorf("no system CA bundle found at any known path; " +
			"SSL_CERT_FILE would then reject every tunneled host. Install the " +
			"platform's ca-certificates package, or point mitm at a directory " +
			"holding a bundle you trust")
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
		// Name the files and the remedy. The CA has a fixed one-year life and
		// no renewal path, so this is reached by simply leaving an install in
		// place — and "not currently valid" alone leaves an operator guessing.
		return nil, nil, fmt.Errorf(
			"MITM CA certificate expired at %s; delete %s and %s to generate a new CA "+
				"(clients trusting the old one must trust the new bundle)",
			cert.NotAfter.UTC().Format(time.RFC3339), certPath, keyPath)
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		return nil, nil, fmt.Errorf("MITM CA certificate is not self-signed: %w", err)
	}
	return cert, key, nil
}

// writePEMAtomic writes a PEM block. ownerOnly marks material that must not
// be readable by anyone else — the private key, not the certificate, which is
// public by construction and has to be readable to be useful.
func writePEMAtomic(path string, ownerOnly bool, blockType string, der []byte) error {
	return writeFileAtomic(path, ownerOnly, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}))
}

func writeFileAtomic(path string, ownerOnly bool, contents []byte) (retErr error) {
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
	// Before the contents, so the private key is never on disk under access
	// this process did not choose. f.Chmod said nothing on Windows, where the
	// file simply inherited the directory's ACL — which loadCA then correctly
	// refused on the very next start, because inherited access can be widened
	// later by a change to the parent.
	if ownerOnly {
		if err := fileperm.Restrict(tmp); err != nil {
			_ = f.Close()
			return err
		}
	} else if err := f.Chmod(0o644); err != nil {
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
	if ownerOnly {
		// Re-applied at the destination: what a rename does to a file's
		// security descriptor is platform-defined, and this file is the one
		// thing here that must not be readable by anyone else.
		if err := fileperm.Restrict(path); err != nil {
			return err
		}
	}
	return nil
}
