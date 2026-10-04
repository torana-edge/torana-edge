# <img src="./assets/logo/torana-color.svg" width="40" align="absmiddle" /> Torana Edge

**Your coding agent. Now programmable.**

Hook into every step between your agent (Claude Code, Codex, Gemini or any
other) and the model: rewrite requests, swap models, transform streams, and add
your own rules with sandboxed plugins. It runs locally, with no account.

[![Release](https://img.shields.io/github/v/release/torana-edge/torana-edge)](https://github.com/torana-edge/torana-edge/releases/latest)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Works with any coding agent](https://img.shields.io/badge/works%20with-any%20coding%20agent-2dd4bf)](docs/HARNESS_SETUP.md)

[Get started](docs/QUICKSTART.md) · [Browse plugins](https://torana.sh/plugins/) ·
[How it works](https://torana.sh/how-it-works/) · [Documentation](docs/README.md)

## Quick start

Install Torana on macOS or Linux. The default installer adds `~/.local/bin`
to your shell profile if needed; use `sh -s -- --no-modify-path` to skip:

```bash
curl -fsSL https://torana.sh/install.sh | sh
```

<details>
<summary>Windows (PowerShell)</summary>

Adds Torana to your user PATH. Pass `-NoModifyPath` to the installer to skip.

```powershell
$installer = Join-Path $env:TEMP ("torana-install-" + [guid]::NewGuid() + ".ps1")
Invoke-WebRequest https://torana.sh/install.ps1 -OutFile $installer
& $installer
Remove-Item -LiteralPath $installer
```

</details>

The installer selects your OS and CPU architecture and verifies the release's
SHA-256 checksum. It does not start Torana. Follow its PATH guidance for your
current terminal; future terminals pick up the saved PATH automatically.
[Inspect the installers](https://github.com/torana-edge/torana-edge/tree/main/scripts)
or [download a binary directly](https://github.com/torana-edge/torana-edge/releases/latest).
No Git or Go is needed to run the proxy. To build it yourself, see
[Contributing](CONTRIBUTING.md#getting-a-build).
The [full quickstart](docs/QUICKSTART.md) includes a first request and harness setup.

Start on a free local port:

```bash
torana start --port 8143
torana status
```

Torana creates its managed configuration automatically. You do not need a
checkout, a seed file, or `TORANA_DATA_DIR` for this walkthrough.
Use any free port. The CLI discovers the running instance, so later commands
and harness setup do not need to repeat it. This guide uses `8143` so the
startup choice is explicit instead of assuming a common local port is free.

### Use the harness you already have

Which coding harness do you use? Keep your existing provider and login. Pick
the [setup recipe for your harness](docs/HARNESS_SETUP.md)—including Claude Code,
Codex, Antigravity, pi, and oh-my-pi.

For an already signed-in Claude Code session, launch it through the default
Anthropic route:

```bash
ANTHROPIC_BASE_URL="$(torana endpoint anthropic)" claude
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
torana open
torana feed --follow
torana stats
torana plugin status
```

`feed --follow` streams new request metadata like `tail -f`. Plain `feed`
prints the latest in-memory snapshot (up to 200 events). Torana does not keep a
full prompt/response traffic log, and the recent feed resets when it restarts.

`start`, `status`, and `stop` print readable summaries; add `--json` when
driving them from scripts or an agent.
`endpoint` prints the running origin, or a provider route such as
`torana endpoint anthropic`, for shell and harness configuration.

## Add one plugin

Install maintained plugins by name—no Git or Go needed. Torana verifies the
release bundle against its checksums and independently published catalogue
digest. You review its permissions before enabling it.

Name-based installs require Torana 0.1.1 or newer. Already on 0.1.0?
Re-run the installer above to upgrade.

Torana plugins run in the request and response path. With permissions you
approve, they can inspect or change a request or response, block it, or call
another endpoint before the workflow continues. That endpoint can be a local
model: your coding agent can keep using its hosted model while a focused local
model handles a narrow job. Combining the two unlocks useful workflows without
moving the whole session to a local model.

### Have a local model—or happy to set up a small one?

Try the contextual `pii` plugin first. It's the best first example of Torana's
local-plus-hosted model workflow: your coding agent keeps its hosted model,
while a small local model checks new tool output before it goes upstream.
You don't need to move the whole agent to a local model. If you need a starting
point, see [Local models](docs/LOCAL_MODELS.md).

First register that model server in Torana:

1. Run `torana open`, open **Settings**, and choose **Add provider**.
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
torana plugin install pii
```

Next, follow **Enable the plugin** below to select that provider for `pii`.
The
[PII guide](https://github.com/torana-edge/torana-plugins/blob/main/plugins/pii/README.md)
has the complete binding and CLI examples.

### Don't have a local model running?

Choose either of these—no scanner setup needed:

**See your traffic with `usage_logger`.** It writes provider, latency and
reported token usage to a private local log, without saving prompts or responses.

```bash
torana plugin install usage_logger
```

**Or try `pii_guard`.** It catches high-confidence PII and common secret
patterns without a model:

```bash
torana plugin install pii_guard
```

`pii_guard` makes no model or network calls. Review its requested tool-result
and state permissions in the local UI before enabling it.

### Enable the plugin

1. Run `torana open`, choose **Pipeline**, and open the plugin you installed.
2. For **pii**, find **Resource bindings and limits** → **scanner**, then select
   `local-scanner` in **Provider**. This is the provider you added and saved in
   Settings—not a new model-server URL. Torana uses its default model and derives
   the inference path; leave **Advanced model settings** closed unless you need
   an override.
3. Review the bundle digest, requested permissions and resource limits. For
   `usage_logger`, review the required `usage.jsonl` file and rotation limits;
   for `pii`, review the scanner's model-call limits.
4. Choose **Approve and enable**. Check that the plugin is enabled in Pipeline.

The install command alone does not enable a plugin, and a rebuilt bundle needs
a new approval.

### Test your setup

#### If you chose usage_logger

Create a small, non-sensitive file in the directory your harness is using:

```bash
echo 'Hello from Torana.' > demo-safe.txt
```

Ask your routed harness: `Read demo-safe.txt and tell me what it contains.`
Open **Live Feed** to see the request, then inspect the content-free usage log
using your shell:

```bash
tail -n 5 "$(torana plugin file path usage_logger usage.jsonl)"
rm demo-safe.txt
```

Expect provider/model, status, latency and reported token counts—not file
contents. `usage_logger` observes traffic; it does not block secrets.
The [usage logger guide](https://github.com/torana-edge/torana-plugins/blob/main/plugins/usage_logger/README.md)
includes CLI enablement and following the log continuously.

#### If you chose pii or pii_guard

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
model request. When the active plugin flags the result, it replaces it with a
recoverable tool error: **Tool output withheld** for `pii`, or **Sensitive output
withheld** for `pii_guard`. That request continues to the primary provider
without the flagged value, so the agent can acknowledge it and move on. With
`pii`, the scanner model decides; this is a useful extra check, not a guarantee
that every secret is caught. Confirm the request in Torana's **Live Feed**,
then remove the test file:

```bash
rm demo-sensitive.txt
```

With `pii_guard` enabled first, this obvious value is replaced before the
contextual scan. With only `pii` enabled, the scanner model decides. The
[full quickstart](docs/QUICKSTART.md#add-one-plugin)
includes the CLI alternatives and troubleshooting detail.

The [plugin listings](https://torana.sh/plugins/) include tool policy, telemetry,
PII checks, schema adaptation and optional compaction. Compaction is a plugin
use case, not a promise of savings:
[read the measured results](https://github.com/torana-edge/torana-plugins/blob/main/plugins/compactor/DEEPSEEK_RESULTS.md).

## Every step you can hook

Plugins run between your agent and the model. They don't intercept local file
access, shell commands or your harness's own permission prompts.

| Step | What a plugin can do |
|---|---|
| Before the request leaves | Rewrite messages and system prompts, add, remove or replace tools, edit tool results, set cache markers, route to a different model or provider, or block the request |
| While it streams back | Transform streamed tokens and tool-call deltas |
| After the response | Record the response, usage and cost, or suggest a model switch |
| Between requests | Run background work such as cache warming |
| From the agent itself | Expose plugin tools your agent can call over MCP |
| Across APIs | Bridge Anthropic, OpenAI and Gemini wire formats with [protocol bridges](docs/PROTOCOL_BRIDGES.md) |

### What that gives you

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

## Configuration

On first start, Torana imports `config.json` into its managed store at
`$TORANA_DATA_DIR/config.json` (or the platform's user-config directory when
that variable is unset). Change a running instance through the CLI or UI,
not by editing the original seed:

```bash
torana config get > settings.json
# Edit the "config" object, preserving "revision".
torana config apply --file settings.json --yes
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

When finished, run `torana stop --yes`. Restore your harness's original
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

If Torana is useful to you, a ⭐ helps others find it.
