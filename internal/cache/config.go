package cache

import (
	"fmt"
	"os"
	"time"
)

// DefaultTTL bounds cross-request plugin state (intents, compacted tool
// results, PII verdicts) in every backend.
const DefaultTTL = 15 * time.Minute

// Config selects and configures the cross-request cache backend.
type Config struct {
	// Backend is "memory" (default) or "redis".
	Backend string `json:"backend,omitempty"`
	// TTLSeconds overrides the default 15-minute entry TTL.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
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
		return NewLocalCache(ttl), nil
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
		store, err := NewRedisStore(addr, password, cfg.Redis.DB, prefix, ttl)
		if err != nil {
			return nil, fmt.Errorf("cache: redis backend %q: %w", addr, err)
		}
		return store, nil
	default:
		return nil, fmt.Errorf("cache: unknown backend %q (want \"memory\" or \"redis\")", cfg.Backend)
	}
}
