package cache

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestRedisTLSVerifiedTransport(t *testing.T) {
	// httptest supplies a test-only certificate valid for 127.0.0.1.
	h := httptest.NewTLSServer(nil)
	cert := h.TLS.Certificates[0]
	leaf := h.Certificate()
	h.Close()
	mr, err := miniredis.RunTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()
	cfg, err := (RedisConfig{TLS: true}).tlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InsecureSkipVerify || cfg.MinVersion < tls.VersionTLS12 {
		t.Fatal("unsafe TLS configuration")
	}
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Backend: "redis", Redis: RedisConfig{Addr: mr.Addr(), TLS: true, CAFile: caPath}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Set(context.Background(), "key", "value")
	if got, ok := s.Get(context.Background(), "key"); !ok || got != "value" {
		t.Fatalf("got %q %t", got, ok)
	}
	if s, err := New(Config{Backend: "redis", Redis: RedisConfig{Addr: mr.Addr(), TLS: true, CAFile: caPath, ServerName: "wrong.invalid"}}); err == nil {
		s.Close()
		t.Fatal("accepted certificate hostname mismatch")
	}
}

func TestRedisTLSConfigRefusesIgnoredOptions(t *testing.T) {
	for _, cfg := range []RedisConfig{{ServerName: "redis.invalid"}, {CAFile: "missing.pem"}, {TLS: true, CAFile: "/does/not/exist/torana-ca.pem"}} {
		if _, err := cfg.tlsConfig(); err == nil {
			t.Fatalf("accepted invalid TLS config: %+v", cfg)
		}
	}
}
