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

### Use the harness you already have

Which coding harness do you use? Keep your existing provider and login. Pick
the [setup recipe for your harness](docs/HARNESS_SETUP.md)—including Claude Code,
Codex, Antigravity, pi, and oh-my-pi.

For an already signed-in Claude Code session, launch it through the example's
Anthropic route:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8080/provider/anthropic claude
```

Ask it to read a small, non-sensitive file, then find the request in Torana's
Feed. The [Claude setup notes](docs/HARNESS_SETUP.md#claude-code) explain how
existing API-key or token settings can take precedence over your login. Your
provider's normal billing and usage limits still apply.

Prefer a direct API request? The [optional API-key example](docs/QUICKSTART.md#optional-use-an-api-key-directly)
uses DeepSeek and explains what to change for your own provider. For a local
model server, follow [Local models](docs/LOCAL_MODELS.md).

Open [the local UI](http://127.0.0.1:8080/_torana/), or keep using the CLI:

```bash
./torana feed
./torana stats
./torana plugin status
```

`start`, `status`, and `stop` print readable summaries; add `--json` when
driving them from scripts or an agent.

## Add one plugin

Torana plugins run in the request and response path. With permissions you
approve, they can inspect or change a request or response, block it, or call
another endpoint before the workflow continues. That endpoint can be a local
model: your coding agent can keep using its hosted model while a focused local
model handles a narrow job. Combining the two unlocks useful workflows without
moving the whole session to a local model.

### **Already have a local model running?**

Try the contextual `pii` plugin first. It catches recognizable sensitive values
directly, then asks your OpenAI-compatible local model about ambiguous tool
output before it reaches the hosted model:

```bash
./torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/pii
```

Open [the local UI](http://127.0.0.1:8080/_torana/), configure its required
`scanner` model service with the local provider and model you already loaded,
then review and enable it. The
[PII guide](https://github.com/torana-edge/torana-plugins/blob/main/plugins/pii/README.md)
has the complete binding and CLI examples.

### **Don't have a local model running?**

Use the deterministic guard instead. It catches high-confidence PII and common
secret shapes without a model:

```bash
./torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/pii_guard
```

`pii_guard` needs only permission to read its tool allowlist and block a
request. Open [the local UI](http://127.0.0.1:8080/_torana/), select
**pii_guard**, review those two permissions, then choose **Approve and enable**.

### Test either choice

Install only one of the two guards; their manifests declare the pair as
conflicting. Both paths now rejoin. Create a file with an obviously synthetic
credential—never use a real key for this check:

```bash
echo 'PAYMENT_API_KEY=sk_test_torana_demo_not_a_real_key_123' > .keys
```

In the coding harness you routed through Torana, enter:

```text
Read the .keys file in this directory and tell me what it contains.
```

The harness reads the file locally and tries to send the tool result in its
next model request. Either guard should stop that request and return a
value-free `sensitive_data_detected` block before the synthetic value reaches
the primary provider. Confirm the blocked request in Torana's **Feed**, then
remove the test file:

```bash
rm .keys
```

This obvious value takes the deterministic fast path in both plugins. The
model-backed `pii` plugin also sends eligible ambiguous content to the local
scanner you configured. The [full quickstart](docs/QUICKSTART.md#add-one-plugin)
includes the CLI alternatives and troubleshooting detail. Installation alone
does not enable a plugin; rebuilding a bundle requires a new approval.

The [plugin listings](https://torana.sh/plugins/) include tool policy, telemetry,
PII checks, schema adaptation and optional compaction. Compaction is a plugin
use case, not a promise of savings:
[read the measured results](https://github.com/torana-edge/torana-plugins/blob/main/plugins/compactor/DEEPSEEK_RESULTS.md).

## Configuration

On first start, Torana imports `config.json` into its managed store at
`$TORANA_DATA_DIR/config.json` (or the platform's user-config directory when
that variable is unset). Change a running instance through the CLI or UI,
not by editing the original seed:

```bash
./torana config get > settings.json
# Edit the "config" object, preserving "revision".
./torana config apply --file settings.json --yes
```

The host validates changes and rejects stale snapshots. Plugin configuration
and order have [separate pipeline commands](docs/CLI.md#plugin-configuration-and-pipeline-order).
[Credential changes](docs/CREDENTIALS.md) require stopping the instance before
writing its on-disk credential store.

For startup overrides and telemetry settings, see the complete
[environment-variable reference](docs/CLI.md#environment-variables).

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
[Plugin examples](https://github.com/torana-edge/torana-plugins) ·
[Contributing](CONTRIBUTING.md) · [Report an issue](https://github.com/torana-edge/torana-edge/issues)

For evaluation, see the [public performance reports](benchmarks/README.md):
both proxy-only overhead and plugin-chain CPU/memory costs are documented.
For deeper operation and development topics, use the [docs index](docs/README.md).
