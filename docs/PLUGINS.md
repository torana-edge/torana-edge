# Running plugins

Torana on its own forwards traffic and does nothing else. Everything
interesting — redaction, telemetry, compaction, schema translation — is a
plugin, and plugins are **not** installed with the gateway. You choose what
runs.

This page is for operating them. If you want to *write* one, that lives with
the SDK: [torana-plugin-sdk](https://github.com/torana-edge/torana-plugin-sdk).

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
Go packages and embedded assets are supported; symlinks and special files are
rejected during bounded staging. You can
read what you are about to run, and the digest is computed from what your own
machine built. This is why a Go toolchain is required.

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

Capabilities are not a sandbox boundary on their own — a manifest is the
plugin's wishlist, and the boundary is *your* approval of a specific digest.
Read the requests before granting them, particularly `env.host_call.*`, which
reaches host functionality, and `env.serve_http`, which lets a plugin vend a
page inside the control plane.

### Capabilities worth pausing over

Most capabilities only let a plugin see or shape a request you were already
making. Three are different in kind, and are worth a second look before you
approve them.

| Capability | What it actually allows |
| --- | --- |
| `env.background_tick` | Runs plugin code on a timer with **no request in flight**. Everything else a plugin does appears in a trace of your own traffic; this does not. |
| `env.host_call.torana_send_request` | Lets the plugin **send its own provider requests**, which costs you money. It cannot choose a destination — only providers you configured — and never sees your credentials, but it can spend within the budget you set. |
| `env.state_set` / `env.state_get` | Durable storage that **survives restarts**, written to disk beside your config. A plugin may keep prompt fragments there. |

None of the three does anything on its own. Ticks are off unless you set an
interval, egress is refused unless you set a budget, and both are refused
outright without the grant. But an approved plugin holding all three can work,
and spend, while you are not looking. Grant them to plugins whose source you have
read.

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

## Ordering

Order matters and Torana enforces the constraints it can:

- `intent` must run before either compactor — both consume its cache.
- Run **one** of `compactor` or `keyword_compactor`, not both.
- Route-capable plugins must precede compaction economic-gate plugins.

A misordering that Torana cannot detect fails quietly — the pipeline runs and
produces less than you configured. Manifests can declare `requires_upstream`
plugin IDs; Torana refuses startup when an approved dependent appears without
its approved dependency earlier in `plugins.order`. `torana plugin list` shows
what is installed; the control plane shows what is actually live.

`plugins.order` is authoritative. Older manifests may contain hook `priority`,
but current hosts ignore it and report a deprecation warning.

## Containers

The runtime image intentionally contains neither git nor a Go compiler. Build
and approve plugins on the host, then mount the resulting bundle directory
read-only at `/plugins` and configure `"plugins": {"dir":"/plugins", ...}`.

## Failure policy

Each plugin declares `pass` or `block`, and you confirm it at approval time.

`pass` means a failing plugin is skipped and the request proceeds. Right for
telemetry or compaction: degraded is better than broken.

`block` means a failing plugin rejects the request. Right for anything
security-shaped — a PII scanner that fails open is worse than no PII scanner.
