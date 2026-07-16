package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisOpTimeout bounds each Redis operation so a slow/unreachable server
// degrades a cache call, never a request.
const redisOpTimeout = 2 * time.Second

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
		client.Close()
		return nil, err
	}
	return &RedisStore{client: client, ttl: ttl, prefix: prefix}, nil
}

func (r *RedisStore) Set(key, value string) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	_ = r.client.Set(ctx, r.prefix+key, value, r.ttl).Err()
}

func (r *RedisStore) Get(key string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	v, err := r.client.Get(ctx, r.prefix+key).Result()
	if err != nil {
		return "", false // miss, expired, or Redis unavailable — all misses
	}
	return v, true
}

func (r *RedisStore) Delete(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
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
