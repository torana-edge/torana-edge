package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisOpTimeout CAPS each Redis operation. It is a ceiling on top of the
// caller's own deadline, not a replacement for it.
//
// The previous comment claimed this bounded a cache call "never a request",
// which was the opposite of what the code did: every operation built its
// context from context.Background(), so a slow server added up to two seconds
// to a LIVE request, and a request the client had already abandoned still
// waited the full two seconds for a cache lookup nobody would read. The
// caller's context now governs; this only stops a hung server from holding a
// request open indefinitely when the caller has no deadline of its own.
const redisOpTimeout = 2 * time.Second

// opContext derives this operation's deadline from the caller's, capped at
// redisOpTimeout. Cancelling the request cancels the cache call with it.
func opContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, redisOpTimeout)
}

// RedisStore is a Redis-backed Store: cross-request plugin state survives
// proxy restarts and is shared across instances in a distributed deployment.
// Errors degrade to cache misses — Redis being down must never take a
// request down.
type RedisStore struct {
	client *redis.Client
	ttl    time.Duration
	prefix string
}

// NewRedisStore connects to Redis and verifies the connection. prefix
// namespaces every key (so one Redis can serve several torana deployments);
// ttl applies per key on Set, matching LocalCache semantics.
func NewRedisStore(addr, password string, db int, prefix string, ttl time.Duration) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db})
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return &RedisStore{client: client, ttl: ttl, prefix: prefix}, nil
}

func (r *RedisStore) Set(ctx context.Context, key, value string) {
	ctx, cancel := opContext(ctx)
	defer cancel()
	_ = r.client.Set(ctx, r.prefix+key, value, r.ttl).Err()
}

func (r *RedisStore) Get(ctx context.Context, key string) (string, bool) {
	ctx, cancel := opContext(ctx)
	defer cancel()
	v, err := r.client.Get(ctx, r.prefix+key).Result()
	if err != nil {
		return "", false // miss, expired, or Redis unavailable — all misses
	}
	return v, true
}

func (r *RedisStore) Delete(ctx context.Context, key string) {
	ctx, cancel := opContext(ctx)
	defer cancel()
	_ = r.client.Del(ctx, r.prefix+key).Err()
}

// Len counts this deployment's keys via SCAN. It exists for tests and
// diagnostics — not a hot path.
func (r *RedisStore) Len() int {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	var n, cursor uint64
	for {
		keys, next, err := r.client.Scan(ctx, cursor, r.prefix+"*", 512).Result()
		if err != nil {
			return int(n)
		}
		n += uint64(len(keys))
		if next == 0 {
			return int(n)
		}
		cursor = next
	}
}

func (r *RedisStore) Close() {
	_ = r.client.Close()
}
