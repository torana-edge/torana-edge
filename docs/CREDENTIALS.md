# Credentials

Torana separates three concerns that are easy to accidentally couple:

1. a provider route names an upstream URL and wire format;
2. a credential ID says which secret should authenticate that route or satisfy
   a plugin slot;
3. a credential source decides how the current secret value is resolved.

Neither providers nor plugins can enumerate sources or read their configuration.
A plugin sees only the bytes for a slot it declared and the operator explicitly
bound while approving that exact bundle.

## Provider authentication

Each provider selects one mode:

```json
{"auth":{"mode":"caller"}}
{"auth":{"mode":"credential","credential":"openai-production"}}
{"auth":{"mode":"none"}}
```

- `caller` forwards the immutable credential captured when the request entered
  Torana. This is the default and the simplest harness setup.
- `credential` resolves the named Torana credential for every upstream attempt.
- `none` sends no credential, normally for a local model.

Torana removes mutable credential headers before routing or failover and then
applies the target provider's mode. A managed credential cannot leak from one
provider to another, and host-originated plugin model-service calls cannot borrow a
caller's credential.

## Built-in sources

Before any `credential set` or `delete` example below, stop the instance.
The complete lifecycle is shown after the examples.

The environment source resolves the variable each time the credential is used:

```bash
export OPENAI_API_KEY='...'
torana credential set openai-production --env OPENAI_API_KEY
```

The local source reads a value without echoing it and stores it encrypted with
Torana's machine-local key:

```bash
torana credential set openai-production
```

List and delete configuration without revealing values:

```bash
torana credential list
torana credential delete openai-production
```

Credential mutations write disk state, not the live API. **Stop before
setting or deleting credentials**, using the same `TORANA_DATA_DIR` as the
instance. Do not write these files while the host owns its in-memory store.

```bash
torana status
torana stop --yes
torana credential set openai-production --env OPENAI_API_KEY
torana start
torana status
```

For an environment-backed value, export the variable before starting Torana;
a running process does not inherit later changes made in your shell. Then
configure the route's auth reference through the live settings API.

## Plugin slots

A plugin manifest declares purpose-specific slots, for example `billing_api`
and `incident_webhook`. During digest approval, the operator binds each slot to
a credential ID. At runtime `sdk.GetCredential("billing_api")` can resolve only
that binding; a plugin cannot ask for `openai-production` by ID or discover what
other credentials exist.

The same plugin may declare ten slots and ten scoped HTTP endpoints if its job
requires ten upstreams. That remains plugin policy, while Torana owns secret
resolution and enforces each approved network destination. Granting both a
credential and an HTTP endpoint lets the plugin transmit that credential, so
the Control Plane presents both in the same bundle-bound approval.

## Custom secret managers

Custom Torana builds can register a source implementation before constructing
the server:

```go
credential.RegisterProvider("vault", func(config json.RawMessage) (credential.Provider, error) {
    return newVaultProvider(config)
})
```

`Provider.Resolve(ctx, key)` is invoked when a value is needed, so an
implementation may fetch or refresh short-lived credentials from AWS Secrets
Manager, HashiCorp Vault, a database, or an internal broker. Plugins and
provider configuration do not change when the backing source changes.

Provider factories are process-wide construction hooks, not a WASM capability.
Register them during startup; hot-loading arbitrary native provider code is
intentionally outside Torana's plugin sandbox.

## Security properties

- configuration contains credential IDs and source keys, never local secret
  plaintext;
- local values are encrypted and written atomically with owner-only modes;
- normal host logs and control-plane responses redact credential values.
  Opt-in [upstream error-body diagnostics](PROTOCOL_BRIDGES.md#diagnose-an-upstream-rejection)
  can contain provider-echoed prompts or secrets; treat those logs as sensitive;
- plugins receive only approved slots and cannot access provider authentication
  configuration;
- scoped HTTP has no ambient proxy, follows no redirects, and enforces the
  approved origin, method, timeout, request/response sizes, rate, and
  concurrency.
