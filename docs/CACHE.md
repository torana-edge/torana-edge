# Cache backend boundaries

The memory backend is the default. Its `max_entries` and `max_bytes` bound
in-process LRU storage; `max_bytes` also rejects a single key+value larger than
that bound. These are memory-backend settings, not per-value quotas shared by
all backends. Redis capacity, eviction, and value limits must be configured
on the Redis server. Entry TTL and request cancellation apply to both stores.

For remote Redis, enable verified TLS in the `cache.redis` configuration:

```json
{
  "backend": "redis",
  "redis": {
    "addr": "redis.example.com:6379",
    "tls": true,
    "password_env": "TORANA_REDIS_PASSWORD"
  }
}
```

TLS uses system trust and a minimum of TLS 1.2. Optional `server_name` selects
the certificate DNS name, and `ca_file` adds a PEM CA bundle for private PKI.
Those options require `tls: true`. There is no verification-bypass option.
Connection or certificate failure is a startup/reconfiguration error, never
a retry using plaintext. Do not place passwords in URLs or checked-in config.

For backward compatibility, `tls` defaults to false. Plaintext Redis exposes
AUTH and cached data to the network; use it only on an appropriately isolated
local connection. Local-first deployment does not make a remote Redis link
private automatically.
