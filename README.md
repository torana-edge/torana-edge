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
./torana start --port 8143
./torana status
```

Use any free port. The CLI discovers the running instance, so later commands
and harness setup do not need to repeat it. This guide uses `8143` so the
startup choice is explicit instead of assuming a common local port is free.

### Use the harness you already have

Which coding harness do you use? Keep your existing provider and login. Pick
the [setup recipe for your harness](docs/HARNESS_SETUP.md)—including Claude Code,
Codex, Antigravity, pi, and oh-my-pi.

For an already signed-in Claude Code session, launch it through the example's
Anthropic route:

```bash
ANTHROPIC_BASE_URL="$(./torana endpoint anthropic)" claude
```

Ask it to read a small, non-sensitive file, then find the request in Torana's
**Live Feed**. The [Claude setup notes](docs/HARNESS_SETUP.md#claude-code) explain how
existing API-key or token settings can take precedence over your login. Your
provider's normal billing and usage limits still apply.

Prefer a direct API request? The [optional API-key example](docs/QUICKSTART.md#optional-use-an-api-key-directly)
uses DeepSeek and explains what to change for your own provider. For a local
model server, follow [Local models](docs/LOCAL_MODELS.md).

Open the running instance's local UI, or keep using the CLI:

```bash
./torana open
./torana feed --follow
./torana stats
./torana plugin status
```

`feed --follow` streams new request metadata like `tail -f`. Plain `feed`
prints the latest in-memory snapshot (up to 200 events). Torana does not keep a
full prompt/response traffic log, and the recent feed resets when it restarts.

`start`, `status`, and `stop` print readable summaries; add `--json` when
driving them from scripts or an agent.
`endpoint` prints the running origin, or a provider route such as
`./torana endpoint anthropic`, for shell and harness configuration.

## Add one plugin

Torana plugins run in the request and response path. With permissions you
approve, they can inspect or change a request or response, block it, or call
another endpoint before the workflow continues. That endpoint can be a local
model: your coding agent can keep using its hosted model while a focused local
model handles a narrow job. Combining the two unlocks useful workflows without
moving the whole session to a local model.

### Comfortable setting up a small local model? Try PII

This is the best way to understand what Torana enables: your hosted coding
model stays in charge, while a small local model checks tool output before
it leaves your machine. Already run an OpenAI-compatible local model server?
Use it with `pii`. Otherwise, start one first and note its URL and model ID.
This is an extra check, not complete protection: models can miss secrets or
flag harmless content. Try both kinds of examples from your own workflow.

First register that model server in Torana:

1. Run `./torana open`, open **Settings**, and choose **Add provider**.
2. Set **Provider name** to `local-scanner`, **Provider format** to `openai`,
   and **Authentication** to **No authentication** for an unauthenticated local server.
3. Set **Upstream URL** to your model server's address—for example,
   `http://127.0.0.1:8081/v1` for a local OpenAI-compatible server, or
   `http://127.0.0.1:11434` for Ollama. Use your server's actual port.
4. If your server requires a model name, open **Plugin model defaults** and
   set **Default model** to its loaded model's ID (for Ollama, its model name).
   Single-model servers that accept requests without a model can leave it blank.
   Leave **Inference path override** blank and choose **Save settings**.

Now install the plugin:

```bash
./torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/pii
```

Run `./torana open` to open the control plane. Go to **Pipeline**, open the
installed **pii** plugin, and find **Resource bindings → scanner → Provider**.
Choose the `local-scanner` provider you added and saved in **Settings** above.
Torana derives the inference path and uses that provider's default model.
The
[PII guide](https://github.com/torana-edge/torana-plugins/blob/main/plugins/pii/README.md)
has the complete binding and CLI examples.

### Prefer to skip local-model setup? Try usage_logger or pii_guard

Start with **usage_logger** to see requests, token usage and timing without
an extra model call:

```bash
./torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/usage_logger
```

Open it in **Pipeline**, then approve and enable it. Make an ordinary request
through your harness and inspect its activity in **Live Feed**. The
[usage_logger guide](https://github.com/torana-edge/torana-plugins/blob/main/plugins/usage_logger/README.md)
shows how to inspect the plugin's local usage log and configure it via CLI.

Or use **pii_guard** for deterministic checks of recognizable secrets, without
a model:

```bash
./torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/pii_guard
```

`pii_guard` makes no model or network calls. Review its requested tool-result
and state permissions in the local UI before enabling it.

### Enable the plugin

Run `./torana open` and select the plugin you
installed. Review its digest and requested permissions. For `pii`, also confirm
the `scanner` binding and model-call limits. Then choose **Approve and enable**.
The install command alone does not enable a plugin, and a rebuilt bundle needs
a new approval.

### Test PII (either guard)

You can enable either plugin on its own, or put `pii_guard` immediately before
`pii` in the pipeline. In that order, the deterministic guard handles obvious
matches first and the model-backed scanner sees subsequent tool output. They do
not share state; `pii` also scans failed tool results because failures can
contain secrets.

Create a file with an obviously synthetic
credential—never use a real key for this check:

```bash
echo 'PAYMENT_API_KEY=sk_test_torana_demo_not_a_real_key_123' > demo-sensitive.txt
```

In the coding harness you routed through Torana, enter:

```text
Read the demo-sensitive.txt file in this directory and tell me what it contains.
```

The harness reads the file locally and includes the tool result in its next
model request. When the scanner flags it, the active guard replaces the result
with a value-free, recoverable tool error. The safe request
continues to the primary provider without the synthetic value, so the agent can
acknowledge it and move on. Confirm the request in Torana's **Live Feed**, then
remove the test file:

```bash
rm demo-sensitive.txt
```

With `pii_guard` enabled first, this obvious value is replaced before the
contextual scan. With only `pii` enabled, the scanner model decides. The
[full quickstart](docs/QUICKSTART.md#add-one-plugin)
includes the CLI alternatives and troubleshooting detail.

If `pii` withholds harmless content, ask your agent to request review using
Torana MCP and the result reference in the error. Open **Approvals** with
`./torana open`, inspect the original content locally, then **Allow upstream**
or **Keep withheld**. The CLI offers `./torana approvals list`,
`./torana approvals show <reference>` and
`./torana approvals approve <reference> --yes`. An allowance applies only to
that exact result in that conversation and plugin bundle—not other reads.
The harness must resend the original result; Torana does not store it.

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
