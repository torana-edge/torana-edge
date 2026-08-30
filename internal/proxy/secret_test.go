package proxy

import (
	"testing"

	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/secret"
)

func newSecretServer(t *testing.T) *Server {
	t.Helper()
	st, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatalf("secret.Open: %v", err)
	}
	return &Server{secrets: st}
}

func TestNormalizeSecretField(t *testing.T) {
	s := newSecretServer(t)

	// raw plaintext -> encrypted token that decrypts back to the input
	got, err := s.normalizeSecretField("sk-raw-key", "")
	if err != nil {
		t.Fatalf("normalize raw: %v", err)
	}
	if !secret.IsEncrypted(got) {
		t.Fatalf("raw key was not encrypted: %q", got)
	}
	if dec, _ := s.secrets.Decrypt(got); dec != "sk-raw-key" {
		t.Fatalf("round-trip mismatch: %q", dec)
	}

	// sentinel -> preserve the currently-stored token unchanged
	if got, _ := s.normalizeSecretField(secretSetSentinel, "enc:STORED"); got != "enc:STORED" {
		t.Fatalf("sentinel should preserve stored value, got %q", got)
	}

	// empty -> cleared
	if got, _ := s.normalizeSecretField("", "enc:STORED"); got != "" {
		t.Fatalf("empty should clear, got %q", got)
	}

	// already-encrypted input -> passed through unchanged (idempotent)
	enc, _ := s.secrets.Encrypt("x")
	if got, _ := s.normalizeSecretField(enc, ""); got != enc {
		t.Fatalf("already-encrypted input should be unchanged")
	}
}

func TestResolveSecret(t *testing.T) {
	s := newSecretServer(t)
	enc, _ := s.secrets.Encrypt("from-enc")

	if got := s.resolveSecret("", enc); got != "from-enc" {
		t.Fatalf("enc resolution: got %q", got)
	}

	// an environment variable takes precedence over the stored secret
	t.Setenv("TORANA_TEST_SECRET", "from-env")
	if got := s.resolveSecret("TORANA_TEST_SECRET", enc); got != "from-env" {
		t.Fatalf("env should win over enc: got %q", got)
	}

	if got := s.resolveSecret("", ""); got != "" {
		t.Fatalf("no source should resolve empty: got %q", got)
	}
}

func TestRedactConfigSecrets(t *testing.T) {
	cfg := provider.Config{}
	cfg.Cache.Redis.PasswordEnc = "enc:REDIS"

	out := redactConfigSecrets(cfg)

	if out.Cache.Redis.PasswordEnc != secretSetSentinel {
		t.Fatalf("redis enc not redacted: %q", out.Cache.Redis.PasswordEnc)
	}
	if cfg.Cache.Redis.PasswordEnc != "enc:REDIS" {
		t.Fatal("redaction mutated the source cache config")
	}
}
