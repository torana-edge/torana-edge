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
torana plugin install ./foo                                   # local directory
torana plugin list
torana plugin remove foo
```

Any git host works. There is no central index to register with and nothing to
publish — `--official` is a convenience alias for a public repository, not a
privileged channel, and it takes the identical code path.

Plugins are **compiled locally from source**, never downloaded prebuilt. You can
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

## Ordering

Order matters and Torana enforces the constraints it can:

- `intent` must run before either compactor — both consume its cache.
- Run **one** of `compactor` or `keyword_compactor`, not both.
- Route-capable plugins must precede compaction economic-gate plugins.

A misordering that Torana cannot detect fails quietly — the pipeline runs and
produces less than you configured. `torana plugin list` shows what is installed;
the control plane shows what is actually live.

## Failure policy

Each plugin declares `pass` or `block`, and you confirm it at approval time.

`pass` means a failing plugin is skipped and the request proceeds. Right for
telemetry or compaction: degraded is better than broken.

`block` means a failing plugin rejects the request. Right for anything
security-shaped — a PII scanner that fails open is worse than no PII scanner.
