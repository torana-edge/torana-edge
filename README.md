# <img src="./assets/logo/torana-color.svg" width="40" align="absmiddle" /> Torana Edge

**Your coding tools. Your models. One place to make them work your way.**

Torana is a local-first, programmable reverse proxy for AI coding agents.
Put it between your harness and model provider to observe requests, apply
policy, or transform traffic with plugins—without tying that work to one harness.

[Get started](docs/QUICKSTART.md) · [Browse plugins](https://torana.sh/plugins/) ·
[How it works](https://torana.sh/how-it-works/) · [Documentation](docs/README.md)

## What you can do

- **See your traffic.** Inspect requests, usage and plugin activity from the
  terminal or the local Web UI.
- **Make your own rules.** Install community source or write a Go/Rust WASM
  plugin for tool policy, metrics, redaction or a workflow-specific transformation.
- **Reuse your plugins.** Plugins work on one shared request/response format
  across supported OpenAI, Anthropic and Gemini APIs.
- **Connect different APIs.** Optional [protocol bridges](docs/PROTOCOL_BRIDGES.md)
  translate supported features between a client and a backend. No plugin needed.
- **Stay in control.** Choose each plugin, inspect its exact build, and approve
  its permissions and resource budgets before enabling it.

Torana runs on your machine, with no Torana account or hosted control service.
Requests still go to the model endpoint you configure. Approved plugins can
also use explicitly bound model services or HTTP endpoints; local-first does
not mean every configured destination is local.

## Quick start

The available install path is a source build. You need Git and Go 1.26.6+.
The [full quickstart](docs/QUICKSTART.md) includes a first request and harness setup.

```bash
git clone https://github.com/torana-edge/torana-edge.git
cd torana-edge
go build -o ./torana ./cmd/torana
cp config.example.json config.json
export TORANA_DATA_DIR="$PWD/.torana-data"
./torana start
./torana status
```

The example config enables no plugins and uses caller-supplied credentials.
Send a request through its DeepSeek route:

```bash
export DEEPSEEK_API_KEY='replace-with-your-deepseek-key'
curl --fail-with-body http://127.0.0.1:8080/provider/deepseek/v1/chat/completions \
  -H "Authorization: Bearer ${DEEPSEEK_API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-flash","messages":[{"role":"user","content":"Reply with exactly: Torana works"}]}'
./torana feed
```

Expect a normal provider response and a matching request in the feed.
The call uses your provider account. For a local endpoint, see
[Local models](docs/LOCAL_MODELS.md).

Open [the local UI](http://127.0.0.1:8080/_torana/), or keep using the CLI:

```bash
./torana stats
./torana plugin status
```

## Add one plugin

Start with `usage_logger`: content-free request, latency and token records
in a rotating private file.

```bash
./torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/usage_logger
./torana plugin inspect usage_logger
```

Follow its [setup guide](https://github.com/torana-edge/torana-plugins/blob/main/plugins/usage_logger/README.md)
to approve the file budget, enable it and inspect a record. Installation alone
does not run code in the proxy. Rebuilding a bundle requires a new approval.

The [catalogue](https://torana.sh/plugins/) includes tool policy, telemetry,
PII checks, schema adaptation and optional compaction. Compaction is a plugin
use case, not a promise of savings:
[read the measured results](docs/DOGFOOD_COMPACTION_RESULTS.md).

## Configuration

Torana imports `config.json` once, then uses its managed store. Change a running
instance through the CLI or UI, not by editing the original seed:

```bash
./torana config get > settings.json
# Edit the "config" object, preserving "revision".
./torana config apply --file settings.json --yes
```

The host validates changes and rejects stale snapshots. Plugin configuration
and order have [separate pipeline commands](docs/CLI.md#plugin-configuration-and-pipeline-order).
[Credential changes](docs/CREDENTIALS.md) require stopping the instance before
writing its on-disk credential store.

## How routing works

A request to `/provider/<name>/<path>` selects a configured backend.
Recognized inference endpoints enter the plugin pipeline; each plugin receives
the shared format rather than vendor-specific JSON. Approved changes are
validated by the host before traffic continues.

Native routes pass auxiliary endpoints through as ordinary HTTP. Routes with
an explicit bridge reject auxiliary endpoints with HTTP 400, including
same-contract bridges. See the [compatibility guide](docs/HARNESS_COMPATIBILITY.md)
and [bridge reference](docs/PROTOCOL_BRIDGES.md) for supported features.

For clients without a usable base-URL override, an optional
[TLS-intercepting ingress](docs/GEMINI_ANTIGRAVITY.md) is available. It is off
unless configured and trusted by the client.

When finished, run `./torana stop --yes`. Restore your harness's original
provider/base URL before using it without Torana.

## Build with us

Try Torana with a request you already make. Hack together a small plugin,
or move a harness-specific tool policy into one you can reuse.

[Plugin SDK](https://github.com/torana-edge/torana-plugin-sdk) ·
[Official plugin sources](https://github.com/torana-edge/torana-plugins) ·
[Contributing](CONTRIBUTING.md) · [Report an issue](https://github.com/torana-edge/torana-edge/issues)

For evaluation, see the [public performance reports](benchmarks/README.md):
both proxy-only overhead and plugin-chain CPU/memory costs are documented.
For deeper operation and development topics, use the [docs index](docs/README.md).
