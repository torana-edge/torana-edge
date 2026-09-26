# Control Torana from a terminal or an agent

The CLI and Web UI use the same local, versioned API. Configuration changes
are validated by the running host, persisted in its managed store, and applied
through the same transaction as a UI save. Do not edit the seed or managed JSON
file to change a running instance.

The examples use `torana` on PATH. With the source-build quickstart, run
`./torana` from the checkout instead. This guide does not install a binary or
change your shell configuration.

## Start, inspect, stop

```bash
torana start
torana status
torana stop --yes
```

`start` runs this binary in the background and waits for its API to report
ready. It is idempotent for the same managed store: it returns the existing
instance instead of spawning a duplicate. `serve` still runs in the foreground.

Both accept `--port` and `--bind` when 8080 is taken or the listener belongs
somewhere else:

```bash
torana serve --port 8143
torana start --port 8143 --bind 127.0.0.1
```

The listener is resolved as `--port`, then `TORANA_PORT`, then the configured
port, then `8080`; `--bind` and `TORANA_BIND` follow the same order over the
`127.0.0.1` default. A flag is the most local statement of intent, so it wins
over an exported environment variable the operator may have forgotten.

An unusable value stops startup rather than falling back — listening on a port
nobody asked for is discovered late and from the wrong end:

```
$ torana serve --port 70000
--port=70000 is outside the valid port range 1-65535
```

`start` checks the endpoint it is about to use before launching, so a port
already serving something else is refused by name rather than producing a child
that dies in the log. That check follows the same precedence, which is what lets
`torana start --port <free>` work while the previously configured port is busy.

On `start`, these apply only when it actually launches an instance. `start`
returns an already-running instance unchanged, and never re-binds it; `status`
and `stop` inspect a running instance and reject both flags. To move a running
listener, change the port in the control plane, which re-binds without a
restart and republishes the address the CLI follows.
The process holds an OS lock for its lifetime, so two processes cannot own the
same store, even on different ports. No OS service or login-startup entry is added.

`start`, `status`, and `stop` print readable summaries. Add `--json` for
structured output, including the instance ID. The startup summary includes PID, version, address, managed
configuration path, and log path. Logs live in `torana.log` beside the managed
configuration, with owner-only permissions (mode bits on Unix, a protected ACL
on Windows). Use `--timeout 90s`
when a larger plugin pipeline needs longer to become ready.

`status` is read-only. It reports `stopped` only on connection refusal with no
live store owner; a failed or starting process is not silently called stopped.
`stop --yes` requests graceful shutdown of the exact inspected instance and
waits for completion (`--timeout 15s` by default). It never kills a cached PID.
An API timeout leaves the outcome uncertain; inspect status before retrying.

## Connect MCP tools

Torana's MCP interface lets a client discover plugin operations and inspect the
proxy through four fixed tools. Turn it on explicitly:

```bash
torana mcp enable --yes
torana mcp status
```

For a Streamable HTTP client, use `http://127.0.0.1:8080/_torana/mcp`
(substitute your port) and configure its Bearer authentication with the token
from `torana mcp token`. That command prints the secret token; keep it in your
client's credential settings, not in a shared chat, screenshot, or repository.
Add `--json` if you need a structured token envelope.

```bash
torana mcp rotate --yes     # prints the replacement token; update your client
torana mcp disable --yes    # closes MCP sessions, retaining the token
```

Enable sets up the token once and prints only enabled status. Repeated token
retrieval keeps the same credential; rotation invalidates the old one. These
commands use the running instance and accept `--addr` like other live commands.

MCP applies the model-facing operation policy. It is separate from the operator
API and is not a sandbox for a harness with unrestricted shell/network access;
see [security boundaries](../SECURITY.md).

## Inspect first

```bash
torana plugin status
torana stats
torana feed
torana conversations --json
torana agent discover
```

Live configuration, plugin, statistics, and feed commands print JSON by default (`--json` is also
accepted). Diagnostics go to stderr; failures exit nonzero. `conversations`
retains its human-readable table unless passed `--json`.

`plugin file path` uses the same instance discovery and prints only the absolute
path returned by the running host. Use your shell to read or follow that file:

```bash
tail -F "$(torana plugin file path usage_logger usage.jsonl)"
```

In PowerShell: `Get-Content -Wait (torana plugin file path usage_logger usage.jsonl)`.
To choose a different local instance, put `--addr` before the plugin name:
`torana plugin file path --addr 127.0.0.1:9090 usage_logger usage.jsonl`.
Torana does not implement `cat`, `tail`, directory listing, or file deletion in
its CLI. The old `plugin files` and `plugin file read/tail/purge` wrappers are
removed. Existing plugin output stays on disk; this change deletes no data.

Use `--addr 127.0.0.1:8080` to select an instance explicitly. Otherwise the CLI
follows the running store owner's recorded listener, including runtime port
changes and overrides from another shell. Requests using this record are bound
to its random instance ID; a stale record cannot authorize a different instance.
When no process owns the store, the CLI reads `TORANA_PORT`, then the managed
configuration, then the seed/default configuration, without creating or changing
a managed store. `TORANA_BIND`
selects the appropriate loopback address, including IPv6. A malformed setting
fails rather than silently selecting another instance.

Only loopback HTTP(S) origins are accepted. The client bypasses environment
HTTP proxies, refuses redirects, and sends `X-Torana-Local-Request: 1` for
deliberate local automation. It does not bypass the server's security checks.
For another machine, use an operator-controlled local tunnel—not a publicly
exposed control plane.

## Environment variables

Set startup options before launching Torana. Starting an already running
instance does not change its environment; stop and restart it to apply new
values. `torana help` lists the same Torana-owned variables.

| Variable | Default | What it controls |
| --- | --- | --- |
| `TORANA_CONFIG` | `config.json` | Seed path; an existing managed store still takes precedence |
| `TORANA_DATA_DIR` | `os.UserConfigDir()/torana` | Directory containing the managed store, `$TORANA_DATA_DIR/config.json` |
| `TORANA_PORT` | Configured port; the example uses `8080` | Override the listener port; `serve --port` and `start --port` win over it |
| `TORANA_BIND` | `127.0.0.1` | Listener address; `serve --bind` and `start --bind` win over it. See the access boundary below |
| `TORANA_DEFAULT_PROVIDER` | Unset | Provider for paths without a `/provider/` prefix |
| `TORANA_PLUGINS_DIR` | `./plugins` | Directory for local plugin-file commands; live administration follows the running host |
| `TORANA_LOG_LEVEL` | Unset | Set `debug` for request-lifecycle logs, also enabled by `--debug` |
| `TORANA_DEBUG_UPSTREAM_ERRORS` | Disabled | Set `1` together with debug logging to log up to 8 KiB of bridged upstream error bodies; these may expose prompts or credentials |
| `TORANA_CI_CACHE` | Unset | Directory for reusing wazero compiled modules across starts |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Unset | OTLP gRPC collector; export is off when neither endpoint is set |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Unset | Metrics-specific endpoint, taking precedence over the general endpoint |
| `OTEL_EXPORTER_OTLP_INSECURE` | `false` | Allow plaintext for a scheme-less endpoint; an explicit `https://` or `http://` scheme wins |
| `OTEL_EXPORTER_OTLP_METRICS_INSECURE` | `false` | Metrics-specific plaintext setting, taking precedence over the general setting |

`os.UserConfigDir()` follows the platform: usually `$XDG_CONFIG_HOME` or
`$HOME/.config` on Linux, `$HOME/Library/Application Support` on macOS, and
`%AppData%` on Windows. `torana status` reports the selected configuration path.

The OTLP exporter applies the other standard OpenTelemetry settings, including
`OTEL_EXPORTER_OTLP_HEADERS`. Torana reads the four variables above when
deciding whether and how to export. Only enable sensitive upstream-error
[diagnostics](PROTOCOL_BRIDGES.md#diagnose-an-upstream-rejection) deliberately.

Widening `TORANA_BIND` exposes the proxy listener, not a multi-user gateway.
The local UI/API still requires a loopback source and host. A reverse proxy
forwarding remote traffic to loopback makes it look local, so that proxy must
block or authenticate `/_torana/*` itself. Conversation state, plugin caches
and control permissions are not tenant-partitioned; keep Torana scoped to your
own workstation and tools.

## Settings: read, edit, apply

```bash
torana config get > settings.json
# Edit settings.json's "config" object; leave "revision" unchanged.
torana config apply --file settings.json --yes
```

The snapshot is `{ "revision": "…", "config": { … } }`. It covers the settings
managed by the host: providers/auth references/fallbacks, protocol bridges,
provider prices and cache policies, cache backend, limits, port, optional MITM ingress, and other
host settings. Secrets have the same redaction as the UI. Preserve a redacted
secret's `__set__` marker to keep its existing value; never treat that marker
as a credential. Named credentials remain available through `torana credential`.

The existing `credential set/delete` commands write the on-disk store, not this
live API. Stop Torana before using them and start it afterward. Changing an auth reference through `config apply` does not refresh values
written by another process.

Plugin configuration is intentionally excluded from this snapshot: settings
apply cannot change the pipeline. An input containing `config.plugins` is
rejected with directions to the pipeline commands.

If another CLI command, the UI, or a restart changes the configuration after
you export it, apply fails with `stale_revision` / HTTP 412. Export a fresh
snapshot and reapply your intended edits after reviewing the changes. Do not
copy a new revision onto an old configuration to bypass the check.
The server also rejects direct API writes without a revision (`revision_required`
/ HTTP 428), so scripts and older browser tabs cannot silently overwrite newer
settings. The CLI includes the revision automatically.

The live administration mutations in this guide require `--yes`; existing
disk-based credential commands retain their own interfaces.
Live mutations are not retried automatically. If a request
times out or the connection drops, inspect the live state before retrying:
the server may already have applied it. After changing the listening port,
the managed instance record points subsequent commands to the new port.
Explicit `--addr` users must specify the new port themselves.

## Protocol bridges

Every [bridge setting](PROTOCOL_BRIDGES.md#configuration) is available through
`config get` and `config apply`: `client`, `upstream`, `model`, `max_tokens`, and
`project`, together with the provider's `format`, URL, auth policy, and fallbacks.
The host applies the same validation and persistence as other configuration
updates; a bridge edit takes effect on subsequent requests without restarting.

Export a fresh settings snapshot, then add this provider entry inside its
`config.providers` object, preserving the other entries and the revision:

```json
"local-messages": {
  "url": "http://127.0.0.1:8000",
  "format": "openai",
  "auth": {"mode": "none"},
  "bridge": {
    "client": "anthropic",
    "upstream": "openai-chat",
    "model": "your-loaded-model"
  }
}
```

Apply it with `torana config apply --file settings.json --yes`. Clients can now
send Anthropic Messages to `/provider/local-messages/v1/messages`, and the local
backend receives Chat Completions. Export a new snapshot before each subsequent
edit. To remove translation while keeping the provider, set its `bridge` member
to `null`; omitting the member preserves the existing bridge. Removing the whole
provider entry removes that route. Use `stats` and `feed` to inspect its traffic.

For sensitive upstream-error details, the diagnostic opt-in works with both
foreground serving and a newly started background instance:

```bash
TORANA_DEBUG_UPSTREAM_ERRORS=1 torana --debug start
```

`torana status` reports the log path. Starting an already running instance does
not change its environment; stop it before restarting with different diagnostic
settings. See [diagnostics](PROTOCOL_BRIDGES.md#diagnose-an-upstream-rejection)
for the 8 KiB limit and what these logs may contain.

## Plugins: install is not approval, and approval is not enablement

`plugin install`, `plugin list`, and `plugin remove` manage local files.
`plugin status` and the commands below inspect or change the running instance,
which may use a different plugin directory. `plugin status` reports that
directory and distinguishes installed, enabled, loaded, stale, and missing
bundles, including the installed and loaded digests.

```bash
torana plugin inspect my-plugin
```

Review the source, requested permissions, exact digest, failure mode, schema,
and resource declarations. Create an approval JSON file describing only that
reviewed bundle. For example, a plugin that requests **only** `env.log` and has
no bound resources would use:

```json
{
  "digest": "sha256:REPLACE_WITH_THE_REVIEWED_BUNDLE_DIGEST",
  "permissions": ["env.log"],
  "failure_mode": "block"
}
```

This is not a universal approval template. Permissions must equal the plugin's
declared set. Plugins that use resources also need the corresponding
`credentials`, `files`, `http_endpoints`, `model_services`, `pricing_resources`,
or `prompt_cache_policies` bindings. The host checks those against the manifest
before saving a changed approval, even for disabled plugins. The schemas and
boundaries are documented in [Running plugins](PLUGINS.md).

```bash
torana plugin approve my-plugin --file approval.json --yes
torana plugin enable my-plugin --yes
torana plugin status
```

`approve` binds the reviewed permissions/resources to that exact digest; it
does not enable a disabled plugin. `enable` requires an existing matching
approval. It never silently grants permissions. An agent should obtain the
operator's consent before granting capabilities to an unfamiliar bundle.

`failure_mode: "pass"` keeps the original request on recoverable plugin errors;
`"block"` fails it. Streaming has stricter terminal-error rules once output has
escaped or the provider stream is incomplete—see the host's plugin semantics.

If the host saves a pipeline but skips plugins, the CLI prints the structured
response, explains the warnings on stderr, and exits nonzero. This means
**saved but not fully running**, not a rolled-back transaction. Inspect
`plugin status` before doing anything else.

```bash
torana plugin disable my-plugin --yes  # keep configuration and approval
torana plugin revoke my-plugin --yes   # disable and remove approval
```

Neither operation deletes source, installed bundles, credentials, or private
plugin data. Rebuilt bundles require review and approval of their new digest.

## Plugin configuration and pipeline order

```bash
torana plugin config get my-plugin > plugin-settings.json
# Edit the "config" object, preserving "revision".
torana plugin config apply my-plugin --file plugin-settings.json --yes

torana pipeline get > pipeline.json
# Edit "pipeline": order, hook_order, config, and approvals.
torana pipeline apply --file pipeline.json --yes

torana pipeline order pii usage_logger --yes
torana pipeline order --empty --yes
```

Plugin settings use the same schema validation as the UI. A pipeline snapshot
keeps settings and approvals for disabled or absent plugins; a simple reorder
does not erase them. `order`, `hook_order`, and `approvals` replace their
respective maps/lists; `config` updates the supplied plugin entries and retains
omitted entries. Per-hook ordering, resource bindings, and operator failure
modes are editable in the pipeline snapshot. An invalid pipeline or changed
approval is rejected by the same host validation used for loading.

`--empty` is required when using the order command to disable everything, so a
missing shell argument cannot accidentally empty your pipeline.

All file-taking commands accept `--file -` for stdin. Review your input before
passing `--yes`; do not put credentials or private configuration in public
issues, shell history, or source control.

## Live request events

```bash
torana feed --follow
```

This prints one compact JSON event per line: a bounded initial snapshot in
chronological order, then live events. Ctrl-C stops it. A dropped stream exits
nonzero rather than silently reconnecting and duplicating the snapshot.
The live feed is bounded telemetry, not a lossless durable audit log.

## Plugin-provided agent operations

```bash
torana agent discover
torana agent call plugin:my-plugin:status
torana agent call plugin:my-plugin:perform-action --file input.json --yes
```

The actual IDs, methods, risk categories, and input/output schemas come from
discovery. Only enabled plugins with an approved `agent.json` descriptor expose
these operations. Writes/destructive operations require `--yes`; input is
validated by the host. Invocation is bound to the discovered bundle digest:
if a reload changes it, discover and review the new operation before retrying.
Built-in mutations use the dedicated revisioned commands, not generic `agent call`.

An arbitrary HTML page is not automatically a machine API. Plugin authors
should expose its automatable actions through `agent.json`, as described in
[Agent-facing control plane](AGENT_CONTROL_PLANE.md).

## UI-to-CLI map

| Control-plane operation | CLI |
| --- | --- |
| Dashboard counters | `stats` |
| Recent/live requests | `feed`, `feed --follow` |
| Conversation metadata | `conversations --json` |
| Provider/cache/limits/ingress settings | `config get`, `config apply` |
| Protocol bridge configuration and removal | `config get`, `config apply` (explicit `bridge: null` to remove) |
| Installed vs loaded bundle details | `plugin status`, `plugin inspect NAME` |
| Digest approval, permissions, bindings, failure mode | `plugin approve NAME --file … --yes`, or pipeline snapshot |
| Enable, disable, revoke | `plugin enable`, `plugin disable`, `plugin revoke` |
| Pipeline/per-hook order and combined edits | `pipeline get`, `pipeline apply`, `pipeline order` |
| Plugin schema/settings | `plugin inspect NAME`, `plugin config get`, `plugin config apply` |
| Advertised plugin agent operations | `agent discover`, `agent call` |
| Private plugin output files | `plugin file path`, composed with shell tools |

Theme selection is a browser preference, not a server configuration change.
