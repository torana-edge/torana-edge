# Running plugins

Torana on its own forwards traffic and does nothing else. Everything
interesting — redaction, telemetry, compaction, schema translation — is a
plugin, and plugins are **not** installed with the gateway. You choose what
runs.

This page is for operating them. If you want to *write* one, that lives with
the SDK: [torana-plugin-sdk](https://github.com/torana-edge/torana-plugin-sdk).

The current host accepts ABI v2 plugins. The SDK repository supports ABI v2
guests in Go and Rust; the first-party plugins use Go, while the Rust crate and
conformance guest demonstrate the same host boundary from a second language.

## Installing

```bash
torana plugin install --official                              # the maintained set
torana plugin install github.com/you/your-plugins/plugins/foo # anyone's repo
torana plugin install github.com/you/your-plugins/plugins/foo@v1.2.0
torana plugin install https://gitlab.example.com/group/subgroup/repo.git//plugins/foo@v1.2.0
torana plugin install ./foo                                   # local directory
torana plugin list
torana plugin remove foo
```

GitHub's `host/owner/repo/subdirectory@ref` shorthand remains supported. For
arbitrary HTTPS hosts and nested GitLab groups, use the unambiguous
`.git//subdirectory@ref` form shown above. A ref may be a branch, tag, or commit
SHA. There is no central index; `--official` is a convenience alias, not a
privileged channel.

Plugins are **compiled locally from source**, never downloaded prebuilt. Nested
Go and Rust packages and embedded assets are supported; symlinks, special
files, and existing toolchain output are rejected or omitted during bounded
staging. You can
read what you are about to run, and the digest is computed from what your own
machine built. Go plugins require the local Go toolchain. Rust plugins require
Rust 1.85+, Cargo, and the `wasm32-wasip1` target:

```bash
rustup target add wasm32-wasip1
torana plugin build ./my-rust-plugin
cargo generate-lockfile      # review and keep the exact dependency graph
torana plugin install ./my-rust-plugin
```

Cargo may execute native `build.rs` programs while compiling—before a WASM
digest exists to approve. Torana therefore refuses one-step installation of a
remote Rust source. Clone it, review the crate and dependency/build-script
surface, and keep a reviewed `Cargo.lock`; installation builds with `--locked`.
Then install the local path. The build pins Cargo to the operator's
active Rust toolchain, so a source-tree `rust-toolchain` file cannot silently
select or download a different compiler. This is intentionally stricter than
the Go remote-source path, whose compiler does not execute package source while
building.

That digest is deliberately a **local approval identity**, not a published
cross-machine reproducibility claim. Official bundles are proven reproducible
across different staging paths with one toolchain, but Go patch versions may
produce different WASM bytes. Go installation sets `GOTOOLCHAIN=local` so source
cannot make your machine download and execute a different compiler before you
have approved anything. Allowing an automatic download to pin the compiler
would weaken that boundary; requiring every developer to preinstall one exact
patch would make installation brittle for a registry digest Torana does not
need. The registry therefore publishes source coordinates and permissions,
not a supposedly universal bundle digest. Two developers may approve different
local digests for the same source, and each approval still binds to the exact
bytes that developer runs.

## Approving

Installing does not enable anything. Torana loads no plugin you have not
approved, and it says so on startup if you have configured one you haven't:

```
plugin pipeline: enabled plugin "schema_translator" was not loaded —
no operator approval for digest sha256:574d412d…
```

In the control plane at `/_torana/`:

1. Open the plugin and read what it requests. A manifest permission is a
   *request*, not a grant.
2. Approve the bundle digest and pick a failure policy.
3. Enable it and set its position in the pipeline.

**Approval binds to the digest.** Rebuild the plugin, change a permission, or add
an `agent.json`, and it must be approved again. That is the point: you approved
one exact artifact, not a name.

## What the sandbox does and does not protect

The sandbox is real. A plugin runs in `wazero` with no filesystem, no network,
and no environment. It sees the request as protobuf and nothing else. That is
what makes it reasonable to run a plugin someone else wrote.

Operator audit files are therefore a host feature, not a plugin capability. A
plugin cannot select a local path or write the audit sink. The host records the
ordered names of plugins it actually invoked and any attributed verdict; see
[Structured audit log](AUDIT_LOG.md).

Capabilities are not a sandbox boundary on their own — a manifest is the
plugin's wishlist, and the boundary is *your* approval of a specific digest.
Read the requests before granting them, particularly `env.host_call.*`, which
reaches host functionality, and `env.serve_http`, which lets a plugin vend a
page inside the control plane.

### Capabilities worth pausing over

Most capabilities only let a plugin see or shape a request you were already
making. Four are different in kind, and are worth a second look before you
approve them.

| Capability | What it actually allows |
| --- | --- |
| `env.background_tick` | Runs plugin code on a timer with **no request in flight**. Everything else a plugin does appears in a trace of your own traffic; this does not. |
| `env.host_call.torana_send_request` | Lets the plugin **send its own provider requests**, which costs you money. It cannot choose a destination — only providers you configured — and never sees your credentials, but it can spend within the budget you set. |
| `env.shared_cache_get` / `env.shared_cache_set` | Crosses the normal plugin-private cache boundary. A granted plugin can read or replace keys intentionally published for other approved plugins. Ordinary `env.cache_*` never does this. |
| `env.state_set` / `env.state_get` | Durable, **plugin-private** storage that survives restarts, written to disk beside your config. A plugin may keep prompt fragments there. |

None of the three does anything on its own. Ticks are off unless you set an
interval, egress is refused unless you set a budget, and both are refused
outright without the grant. But an approved plugin holding all three can work,
and spend, while you are not looking. Grant them to plugins whose source you have
read.

### Instance concurrency and idle memory

Each plugin may execute on up to four WASM instances concurrently by default.
Torana keeps one instance ready, grows lazily when concurrent requests need
more, and retires the burst-created idle instances after at least one quiet
minute. This preserves immediate low-traffic response while avoiding permanent
retention of a previous traffic spike.

The timeout is configurable, and an explicit zero disables retirement for an
operator who prefers to keep every burst-created instance warm:

```json
{ "plugins": { "runtime": { "instance_idle_timeout_seconds": 120 } } }
```

`pool_size` remains the concurrency ceiling, not a promise to preallocate or
permanently retain that many instances. Lowering it can reduce memory but also
limits parallel plugin execution; measure the workload before changing it. A
nonzero idle timeout must be between 10 seconds and 24 hours.

### Turning on background ticks

Ticks are off by default. A plugin that declares `run_on_tick` still does
nothing until you choose a cadence:

```json
{ "plugins": { "runtime": { "tick_interval_seconds": 60 } } }
```

The floor is 10 seconds. Every declaring plugin is woken on the same tick, so
this is a global cadence, not a per-plugin one; a plugin that wants to act less
often counts ticks itself.

### Budgeting plugin egress

A plugin that can send provider requests still cannot send any until you say how
much it may spend:

```json
{
  "plugins": {
    "runtime": {
      "egress": {
        "cache_warmer": { "max_calls_per_minute": 4, "max_tokens_per_hour": 200000 }
      }
    }
  }
}
```

Budgets are per plugin and roll over a window, so a throttled plugin recovers
rather than being locked out. `max_calls_per_minute` is the hard ceiling.
`max_tokens_per_hour` is a backstop for calls that are individually cheap but
collectively large; it is checked against tokens already spent, since a call's
cost is not knowable before making it, so it can overshoot by one call.

Every plugin-originated request appears in the live feed marked
`plugin-egress` and attributed to the plugin, and `/stats` counts calls and
refusals per plugin. If a plugin is spending, you can see it.

### Plugin telemetry limits

Plugin-defined counter, metric, and label names use the ASCII grammar
`[A-Za-z0-9][A-Za-z0-9._-]{0,63}`. Each plugin may retain at most 64 `/stats`
counter names and 64 OTel metric names; each OTel metric may retain at most 64
distinct label sets, with at most eight labels per set. Label values must be
valid UTF-8 and at most 128 bytes. The `plugin` label and host-owned Torana
instrument names are reserved, so a guest cannot spoof attribution or collide
with platform telemetry.

These are lifetime bounds, not rolling caches: allowing entries to churn would
still create unbounded OTel aggregations. A rejected `/stats` update increments
that plugin's fixed `_rejected_updates` counter. A rejected OTel update
increments `torana_plugin_metric_rejections_total{plugin,reason}`. Guest-chosen
names or values are never copied into those overflow signals.

## Ordering

Order matters and Torana enforces the constraints it can:

- When enabled, `intent` must precede a compactor. Its cache improves relevance,
  but both compactors derive bounded local guidance when it is absent.
- Run **one** of `compactor` or `keyword_compactor`, not both. Their manifests
  declare the conflict, so Torana rejects an approved generation containing
  both before either guest is loaded.
- Put `tool_governor` before `intent` and `schema_translator`. Governance is
  defined over the harness's original tool definitions; the later plugins may
  then add intent fields or translate an approved schema for the provider.
- Route-capable plugins must precede compaction economic-gate plugins.

A misordering that Torana cannot detect fails quietly — the pipeline runs and
produces less than you configured. Manifests can declare `requires_upstream`
plugin IDs; Torana refuses startup when an approved dependent appears without
its approved dependency earlier in `plugins.order`. `torana plugin list` shows
what is installed; the control plane shows what is actually live.

Manifests can also declare `conflicts_with` stable plugin IDs. If either exact
approved bundle names the other, they cannot run in the same pipeline
generation; order does not resolve the conflict. Unapproved or otherwise
skipped bundles do not block an approved plugin from running.

`plugins.order` remains authoritative for loading, lifecycle, and the default
execution order. A plugin cannot assign itself priority. When an operator needs
different observation policy for a particular multi-plugin hook, use an exact
`plugins.hook_order` override:

```json
{
  "plugins": {
    "order": ["audit", "policy", "observer"],
    "hook_order": {
      "run_after_response": ["policy", "observer", "audit"]
    }
  }
}
```

An override must list every plugin named in `plugins.order` that declares that
hook exactly once, even while a bundle awaits approval; skipped plugins simply
do not appear in that immutable live generation and the remaining relative
order is preserved.
Missing, extra, duplicate, disabled, or wrong-hook names fail the configuration
instead of being appended implicitly. Supported keys are
`run_before_request`, `run_after_response`, `run_on_stream_chunk`, and
`run_on_tick`; `run_on_http_request` is target-addressed and cannot be ordered.
Declared upstream dependencies and the route-before-compaction economic rule
still apply to the effective hook order. A hot reload builds immutable hook
orders with the new plugin generation and swaps them atomically, so an
in-flight request never observes half an order change.

Plugins may declare optional `minimum_torana_version` and
`maximum_torana_version` bounds. A tagged host with a semantic version enforces
them. Development and commit-SHA builds have no reliable product version, so
they log and skip this product-version gate while still enforcing
`abi_version`, hooks, permissions, exports, and approvals.

## Failure policy

Each plugin declares `pass` or `block`, and you confirm it at approval time.

`pass` means a failing plugin is skipped and the request proceeds. Right for
telemetry or compaction: degraded is better than broken.

`block` means a failing plugin rejects the request. Right for anything
security-shaped — a PII scanner that fails open is worse than no PII scanner.

One guest invocation is bounded by a five-second host timeout. A timeout or
trap follows that plugin's approved `pass`/`block` policy; it is not retried.
Before-request hooks delay the provider request, and stream/after-stream hooks
delay completion of the client response because Torana must keep request state
alive until the stream pipeline finishes. In other words, observational stream
plugins are still on the client's critical path. If Torana itself is down, the
client sees an ordinary connection failure unless the client has a separately
configured direct-provider fallback; Torana does not silently bypass itself.

## Format-specific provider extensions

Each format has a documented per-format envelope grammar for its
provider-visible unparsed fields. The Gemini/Code Assist two-scope
envelope (outer-wrapper extras + the structural `request` member holding
inner-request extras, with the canonical members FORBIDDEN as extras and
rebuilt from the canonical ABI fields) is specified in
`GEMINI_ANTIGRAVITY.md`; replacements that smuggle canonical members
through the extras path are plugin-output invalidity (`pass` rolls back,
`block` refuses). The host-only topology facts (format variant, Code
Assist flag, Responses layout) are typed and never part of the ABI.
