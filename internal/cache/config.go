package cache

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"
)

// DefaultTTL bounds cross-request plugin state (intents, compacted tool
// results, PII verdicts) in every backend.
const DefaultTTL = 15 * time.Minute

const (
	DefaultMaxEntries = 10_000
	DefaultMaxBytes   = 64 << 20 // 64 MiB
)

// Config selects and configures the cross-request cache backend.
type Config struct {
	// Backend is "memory" (default) or "redis".
	Backend string `json:"backend,omitempty"`
	// TTLSeconds overrides the default 15-minute entry TTL.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// MaxEntries and MaxBytes bound the in-process LRU cache. Zero selects the
	// defaults. Redis deployments should configure their server-side eviction
	// policy separately. MaxBytes also limits individual key+value admission
	// only in memory; it is not a cross-backend per-value quota.
	MaxEntries int `json:"max_entries,omitempty"`
	MaxBytes   int `json:"max_bytes,omitempty"`
	// Redis configures the redis backend.
	Redis RedisConfig `json:"redis,omitempty"`
}

// RedisConfig points at a Redis server for distributed / restart-safe state.
type RedisConfig struct {
	Addr string `json:"addr,omitempty"` // host:port, default "127.0.0.1:6379"
	// PasswordEnv names an environment variable holding the Redis password
	// (never put the password itself in the config file).
	PasswordEnv string `json:"password_env,omitempty"`
	PasswordEnc string `json:"password_enc,omitempty"`
	Password    string `json:"-"`
	DB          int    `json:"db,omitempty"`
	// Prefix namespaces this deployment's keys. Default "torana:".
	Prefix string `json:"prefix,omitempty"`
	// TLS enables verified transport encryption (TLS 1.2 or newer).
	TLS bool `json:"tls,omitempty"`
	// ServerName overrides the certificate DNS name; CAFile adds trusted PEM CAs.
	ServerName string `json:"server_name,omitempty"`
	CAFile     string `json:"ca_file,omitempty"`
}

func (c RedisConfig) tlsConfig() (*tls.Config, error) {
	if !c.TLS {
		if c.ServerName != "" || c.CAFile != "" {
			return nil, fmt.Errorf("redis server_name and ca_file require tls=true")
		}
		return nil, nil
	}
	result := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read redis CA: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("redis ca_file contains no certificates")
		}
		result.RootCAs = roots
	}
	return result, nil
}

// New builds the configured Store. An empty config yields the in-memory
// backend with the default TTL. A misconfigured or unreachable Redis is a
// hard error — a deployment that asked for distributed state must not
// silently fall back to per-process memory.
func New(cfg Config) (Store, error) {
	ttl := DefaultTTL
	if cfg.TTLSeconds > 0 {
		ttl = time.Duration(cfg.TTLSeconds) * time.Second
	}
	switch cfg.Backend {
	case "", "memory":
		maxEntries := cfg.MaxEntries
		if maxEntries <= 0 {
			maxEntries = DefaultMaxEntries
		}
		maxBytes := cfg.MaxBytes
		if maxBytes <= 0 {
			maxBytes = DefaultMaxBytes
		}
		return NewLocalCacheWithLimits(ttl, maxEntries, maxBytes), nil
	case "redis":
		addr := cfg.Redis.Addr
		if addr == "" {
			addr = "127.0.0.1:6379"
		}
		password := cfg.Redis.Password
		if password == "" && cfg.Redis.PasswordEnv != "" {
			password = os.Getenv(cfg.Redis.PasswordEnv)
		}
		prefix := cfg.Redis.Prefix
		if prefix == "" {
			prefix = "torana:"
		}
		tlsConfig, err := cfg.Redis.tlsConfig()
		if err != nil {
			return nil, err
		}
		store, err := newRedisStore(addr, password, cfg.Redis.DB, prefix, ttl, tlsConfig)
		if err != nil {
			return nil, fmt.Errorf("cache: redis backend %q: %w", addr, err)
		}
		return store, nil
	default:
		return nil, fmt.Errorf("cache: unknown backend %q (want \"memory\" or \"redis\")", cfg.Backend)
	}
}
