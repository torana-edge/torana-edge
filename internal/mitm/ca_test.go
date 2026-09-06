package mitm

import (
	"crypto/x509"
	"strings"
	"testing"
	"time"
)

// A cached leaf must be re-minted before it expires.
//
// Leaves live 24h and the cache never evicted, so a proxy left running
// overnight served an expired certificate to every subsequent handshake and the
// ingress simply stopped working — with a TLS error that points at the client
// rather than here. No test covered it, which is why it survived.
func TestLeafForReMintsBeforeExpiry(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}

	base := time.Now()
	ca.now = func() time.Time { return base }

	first, err := ca.LeafFor("api.example.com")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}

	// Well inside the lifetime: the same certificate is reused.
	ca.now = func() time.Time { return base.Add(time.Hour) }
	again, err := ca.LeafFor("api.example.com")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	if again != first {
		t.Error("a leaf with hours of life left should be served from cache")
	}

	// Past the renewal threshold: a fresh certificate, still valid.
	ca.now = func() time.Time { return base.Add(leafLifetime - leafRenewBefore/2) }
	renewed, err := ca.LeafFor("api.example.com")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	if renewed == first {
		t.Fatal("a leaf inside the renewal window must be re-minted, not reused")
	}
	leaf, err := x509.ParseCertificate(renewed.Certificate[0])
	if err != nil {
		t.Fatalf("parse renewed leaf: %v", err)
	}
	now := base.Add(leafLifetime - leafRenewBefore/2)
	if now.After(leaf.NotAfter) {
		t.Errorf("renewed leaf is already expired: NotAfter=%s now=%s", leaf.NotAfter, now)
	}

	// The regression itself: a day later the served certificate must still be
	// valid at the moment it is served.
	ca.now = func() time.Time { return base.Add(48 * time.Hour) }
	later, err := ca.LeafFor("api.example.com")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	leaf, err = x509.ParseCertificate(later.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if base.Add(48 * time.Hour).After(leaf.NotAfter) {
		t.Error("served an EXPIRED leaf after the cache entry aged out — the ingress breaks here")
	}
}

// Two hosts must not share a serial, and a serial must not encode mint time.
func TestLeafSerialsAreDistinct(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	seen := map[string]bool{}
	for _, host := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		cert, err := ca.LeafFor(host)
		if err != nil {
			t.Fatalf("LeafFor(%s): %v", host, err)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatalf("parse %s: %v", host, err)
		}
		s := leaf.SerialNumber.String()
		if seen[s] {
			t.Errorf("serial %s reused across hosts", s)
		}
		seen[s] = true
	}
}

// A leaf can never outlive the CA that signed it. Near the CA's one-year
// expiry a nominal 24h leaf produced a chain that failed at the client long
// before the leaf itself lapsed, and past the expiry LeafFor kept minting
// unusable certificates forever — loadCA's expiry check runs at startup and is
// never revisited by a long-running process.
func TestLeafValidityIsCappedByTheCA(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	caExpiry := ca.cert.NotAfter

	// Six hours of CA life left: the leaf's nominal 24h must be capped.
	near := caExpiry.Add(-6 * time.Hour)
	ca.now = func() time.Time { return near }
	cert, err := ca.LeafFor("api.example.com")
	if err != nil {
		t.Fatalf("LeafFor near CA expiry: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if leaf.NotAfter.After(caExpiry) {
		t.Errorf("leaf outlives its signer: leaf NotAfter=%s, CA NotAfter=%s — the chain "+
			"fails at the client while the leaf still looks valid here",
			leaf.NotAfter.UTC(), caExpiry.UTC())
	}
	if !leaf.NotAfter.Equal(caExpiry) {
		t.Errorf("leaf NotAfter = %s, want the CA's %s", leaf.NotAfter.UTC(), caExpiry.UTC())
	}

	// Too little CA life to mint anything usable: refuse, and say what to do.
	ca.now = func() time.Time { return caExpiry.Add(-leafMinLifetime / 2) }
	if _, err := ca.LeafFor("api.example.com"); err == nil {
		t.Fatal("minted a leaf with less than leafMinLifetime of CA life remaining")
	} else {
		for _, want := range []string{"ca-cert.pem", "ca-key.pem", "delete"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("expiry error does not tell the operator what to do (missing %q): %v", want, err)
			}
		}
	}

	// Past the CA's expiry the answer is the same error, not a certificate.
	ca.now = func() time.Time { return caExpiry.Add(time.Hour) }
	if _, err := ca.LeafFor("other.example.com"); err == nil {
		t.Fatal("minted a leaf from an EXPIRED CA")
	}
}
