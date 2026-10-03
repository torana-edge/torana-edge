# Torana Edge — Quickstart

Get a request flowing through Torana, see it in the feed, then add one plugin.
Torana sits between your coding harness and model provider so your plugins can
work across supported APIs.

## Prerequisites

Which coding harness do you already use? Start with its existing provider/login
using the [harness setup guide](HARNESS_SETUP.md). A separate API key is only
needed if you choose the direct API-key example.

## Install Torana

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
[Contributing](../CONTRIBUTING.md#getting-a-build).

## Start without plugins

```bash
torana --debug start --port 8143
torana status
```

Use any free port. The CLI discovers the running instance for later commands.
Torana runs in the background on loopback and creates its managed configuration
in your platform's [user-config directory](CLI.md#environment-variables).
No checkout or `TORANA_DATA_DIR` export is required. This directory contains
private configuration, credentials and plugin state; keep it out of source control.
Default provider routes use harness credentials and no plugins are enabled.

## Connect your harness

For an already signed-in Claude Code installation:

```bash
ANTHROPIC_BASE_URL="$(torana endpoint anthropic)" claude
```

Ask it to read a small non-sensitive file, then run `torana open` and inspect
the local control plane's **Live Feed**. Keep the `anthropic` provider's
authentication set to **Use harness credentials**; no DeepSeek key is needed.
For Codex, Antigravity, pi, or oh-my-pi, use the
[harness-specific settings and verification results](HARNESS_SETUP.md).

You can also inspect activity from the terminal:

```bash
torana feed --follow
```

`feed --follow` streams new request metadata like `tail -f`. Plain `feed`
prints the latest in-memory snapshot (up to 200 events). Torana does not retain
a full prompt/response traffic log, and the recent feed resets on restart.

## Optional: use an API key directly

Already have an API key and want to make a request without a harness?
Here is a DeepSeek example. For another provider, modify its URL, format and
authentication in Settings, then adapt the request endpoint, model, and payload
to that provider’s API. Choose a model available to your account.

```bash
export DEEPSEEK_API_KEY='replace-with-your-deepseek-key'
TORANA_URL="$(torana endpoint)"

curl --fail-with-body "$TORANA_URL/health"

curl --fail-with-body "$(torana endpoint deepseek)/v1/chat/completions" \
  -H "Authorization: Bearer ${DEEPSEEK_API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-flash","messages":[{"role":"user","content":"Reply with exactly: Torana works"}]}'
```

The health endpoint returns `{"status":"ok"}` and the second command returns a
normal provider response. Run `torana feed` to find the matching request;
`torana status` reports the log path. Debug logging records safe
request-received/completed lines, not headers or bodies. Bridge error-body
diagnostics have a separate [explicit opt-in](PROTOCOL_BRIDGES.md#diagnose-an-upstream-rejection).

## Configure

The built-in defaults include native Anthropic, OpenAI, Gemini, and DeepSeek
routes, with no plugins enabled. Choose the route for your harness; you do not
need an account with every listed provider. For a ChatGPT login or Antigravity,
follow the [harness guide](HARNESS_SETUP.md) for the appropriate endpoint and setup.

`limits.concurrency` is the maximum number of simultaneous upstream requests
per identity; `limits.rpm` is a per-identity token bucket refilled over one
minute. Set either value to `0` to disable that limit. Standard JSON does not
support comments, so the shipped file stays directly parseable; these field
descriptions and the hints in the local Control Plane are the annotated
reference.

For custom deployments, [config.example.json](../config.example.json) is an optional
first-run seed. Torana imports a seed into its managed store at
`$TORANA_DATA_DIR/config.json`, or the platform's
[user-config directory](CLI.md#environment-variables) when unset. After that,
the managed store is authoritative so Control Plane edits survive restarts.
Changing the original seed does not overwrite managed state; Torana logs a
warning when both files exist and differ. Run `torana open` to edit the
managed configuration. Remove the managed store only if you deliberately want
the next start to re-import the seed. `TORANA_CONFIG` selects a different
seed path; it does not bypass an existing managed store.

For repeated first-run testing, point `TORANA_DATA_DIR` at a new empty directory
for each run. That avoids accidentally exercising an older managed
configuration while believing you are testing a changed seed.

## Add one plugin

Maintained plugins install by name from verified release bundles. No Git or
Go is needed. Installation does not approve permissions or enable a plugin.

The empty plugin order is intentional: discovered plugins are not implicitly
trusted or enabled. After the plugin-free request above succeeds, leave Torana
running and choose one plugin. The watcher discovers the new bundle without
a restart.

Plugins run in the request and response path. With permissions you approve,
they can inspect or change a request or response, block it, or call another
endpoint. That lets your harness keep using its hosted model while a focused
local model handles a narrow job.

### Have a local model—or happy to set up a small one?

Try `pii` first: it's the best showcase of combining a focused local model with
your hosted coding model. The local scanner checks new tool output before the
hosted model sees it, without moving your whole workflow to local inference.
Use Ollama or another OpenAI-compatible local server; see
[Local models](LOCAL_MODELS.md) if you need setup guidance.

```bash
torana plugin install pii
torana plugin list
```

Register your local model before selecting it in the plugin:

1. Run `torana open`, open **Settings**, and choose **Add provider**.
2. Set **Provider name** to `local-scanner`, **Provider format** to `openai`,
   and **Authentication** to **No authentication** for an unauthenticated local server.
3. Set **Upstream URL** to the local server's address, such as
   `http://127.0.0.1:8081/v1`, or `http://127.0.0.1:11434` for Ollama.
   Use your actual model-server port.
4. If the server requires a model name, open **Plugin model defaults** and
   set **Default model** to its loaded model ID. Single-model servers that
   accept requests without a model can leave it blank. Leave **Inference path
   override** blank and choose **Save settings**.

Next, follow **Enable the plugin** below to choose `local-scanner` for `pii`.
Eligible tool output goes to this scanner; selecting a remote scanner would
send that output to the remote endpoint instead.

The [model-backed PII guide](https://github.com/torana-edge/torana-plugins/blob/main/plugins/pii/README.md)
includes the exact CLI configuration and approval document.

### Don't have a local model running?

Choose either option—neither needs a scanner model:

**See your traffic with usage_logger.** It records provider, latency and
reported token usage locally, without saving prompts or response contents.

```bash
torana plugin install usage_logger
```

**Or try pii_guard.** It checks high-confidence PII and common secret patterns
deterministically, without a model or network call.

```bash
torana plugin install pii_guard
```

The
[deterministic guard guide](https://github.com/torana-edge/torana-plugins/blob/main/plugins/pii_guard/README.md)
also covers configuration through the CLI.

### Enable the plugin

1. Run `torana open`, choose **Pipeline**, and open the plugin you installed.
2. For **pii**, find **Resource bindings and limits** → **scanner** and choose
   `local-scanner` in **Provider**. This selects the provider you saved in
   Settings. Torana uses its default model and derives the inference path;
   leave **Advanced model settings** closed unless you need an override.
3. Review the digest, requested permissions and resource limits. For
   `usage_logger`, review its required `usage.jsonl` file and rotation limits;
   for `pii`, review its scanner's model-call limits.
4. Choose **Approve and enable**, then confirm the plugin is enabled in Pipeline.

Installing a plugin does not enable it. A rebuilt bundle needs a new digest approval.

### Test your setup

#### If you chose usage_logger

Create a small, non-sensitive file in the directory your harness is using:

```bash
echo 'Hello from Torana.' > demo-safe.txt
```

Ask your routed harness: `Read demo-safe.txt and tell me what it contains.`
Open **Live Feed** to find its request, then read the content-free records
with your shell:

```bash
tail -n 5 "$(torana plugin file path usage_logger usage.jsonl)"
rm demo-safe.txt
```

Expect provider/model, status, latency and reported token counts—not the file
contents. This plugin observes traffic; it does not block secrets.
The [usage logger guide](https://github.com/torana-edge/torana-plugins/blob/main/plugins/usage_logger/README.md)
has CLI enablement and continuous log-following examples.

#### If you chose pii or pii_guard

Create a file containing an obviously synthetic
credential. Do not use a real key:

```bash
echo 'PAYMENT_API_KEY=sk_test_torana_demo_not_a_real_key_123' > demo-sensitive.txt
```

In the coding harness you routed through Torana, enter:

```text
Read the demo-sensitive.txt file in this directory and tell me what it contains.
```

The harness will read the file locally and include the tool result in its next
model request. When the plugin flags the result, it replaces it with a
recoverable tool error: **Tool output withheld** for `pii`, or **Sensitive output
withheld** for `pii_guard`. That request continues to the primary provider
without the flagged value, so the agent can acknowledge it and move on.
`pii` is an extra model-backed check, not a guarantee that every secret is
caught. You can inspect the request in Torana's **Live Feed**.

With only `pii`, the scanner model decides. You can also put `pii_guard` directly
before `pii` in Pipeline: the deterministic guard handles obvious matches first.
They do not share state; `pii` also checks failed tool results, since failures
can contain secrets. After the check, remove the test file:

```bash
rm demo-sensitive.txt
```

Installation alone never approves, enables, or runs anything. The installer
also accepts a reviewed local plugin directory or another repository URL.

Software agents and shell scripts can discover the same guarded control-plane
capabilities as JSON:

```bash
curl --fail-with-body --silent \
  "$(torana endpoint)/_torana/api/v1/" | jq
```

See [AGENT_CONTROL_PLANE.md](AGENT_CONTROL_PLANE.md) for stable operation IDs,
JSON error envelopes, mutation guidance, and plugin-contributed operations.

Browse the [plugin setup guides](https://github.com/torana-edge/torana-plugins#choose-a-plugin)
when you want to change traffic. No plugin transformation is enabled by default.


## Provider authentication and fallbacks

The shipped seed names these native routes. Replace or add providers through
the CLI or UI after the first import; the route name is your local identifier,
not a claim that every feature of that provider is supported.

| Route prefix | Upstream URL | Format |
| --- | --- | --- |
| `/provider/deepseek/...` | `https://api.deepseek.com` | `openai` |
| `/provider/deepseek-anthropic/...` | `https://api.deepseek.com/anthropic` | `anthropic` |
| `/provider/openai/...` | `https://api.openai.com` | `openai` |
| `/provider/anthropic/...` | `https://api.anthropic.com` | `anthropic` |
| `/provider/gemini/...` | `https://generativelanguage.googleapis.com` | `gemini` |

Provider `url` values contain an HTTP(S) origin and optional path only. Query
strings, fragments, and embedded userinfo are rejected rather than silently
ignored. Supply required query parameters (such as an API version) on each
request path; provider-level query defaults are not supported. Use `auth` for
credentials. Native routes require fallbacks with the same format, including
transparent mode: an empty format cannot fall back to a named adapter, or vice
versa. Explicit [protocol bridges](PROTOCOL_BRIDGES.md) can translate to
configured fallback contracts; incompatible features are refused.

Bodies larger than the retry buffer are sent intact to the primary once, with
fallback disabled and a diagnostic log entry. This is not an additional
outbound body-size rejection.

Every provider has one authentication mode:

- `caller` (the default when omitted) forwards the immutable credential
  captured when that request entered Torana;
- `credential` resolves a named Torana credential and injects it host-side;
- `none` sends no credential, useful for a local model.

Query parameters named `key`, `api_key`, `api-key`, and `access_token` are
reserved for authentication. Torana strips them in `credential` and `none`
mode, including during routing and failover. In `caller` mode it restores
them from the ingress snapshot. Other query parameters keep their original
ordering and encoding.
This policy applies to intercepted caller traffic. Plugin-originated provider
requests retain their explicitly supplied query fields and use managed header
authentication (or no authentication). Raw semicolon-containing components in
intercepted queries are discarded because providers disagree on their parsing;
encode a literal semicolon in a parameter value as `%3B`.

The full source, slot, refresh, and custom-provider model is documented in
[Credentials](CREDENTIALS.md).

Configure a reusable environment-backed credential without putting its value in
JSON. In the same data-directory environment, stop before changing disk state:

```bash
torana stop --yes
torana credential set fallback-api-key --env FALLBACK_API_KEY
# Export FALLBACK_API_KEY in this shell before starting the host.
torana start --port 8143
torana status
```

Credential commands are disk-based; they do not refresh a running instance.
See [Credentials](CREDENTIALS.md) for the stop-before-write lifecycle.

Then set the fallback provider's auth to
`{"mode":"credential","credential":"fallback-api-key"}`.

A cross-vendor fallback should use a credential of its own. Torana always
removes credentials already installed on the mutable upstream request, then
rebuilds authentication from the fallback's explicit mode. Use `caller` on a
fallback only when the operator has deliberately established that it accepts
the original caller credential (for example, another endpoint of the same
vendor).

If the fallback is a local server that ignores credentials, use `none`. Torana
never guesses that two providers may share a credential.

A fallback with neither is reported at startup, because the failure it produces
otherwise is a 401 from a provider you never called directly.


## Route your harness

Torana sends supported inference endpoints through its shared format and
plugins. Start with the [harness setup guide](HARNESS_SETUP.md), which owns the
connection recipes and their verification details. For a backend with a
different API, use a [protocol bridge](PROTOCOL_BRIDGES.md). The
[compatibility reference](HARNESS_COMPATIBILITY.md) explains inference and
auxiliary routing.

### omp (oh-my-pi)
Keep the provider you already use and change its base URL to the matching
Torana route. See [pi and oh-my-pi setup](HARNESS_SETUP.md#pi-and-oh-my-pi).

### Claude Code
Use your existing login through the native Anthropic route. See
[Claude Code setup](HARNESS_SETUP.md#claude-code).

### Antigravity CLI (agy)
Use the [local TLS-ingress guide](GEMINI_ANTIGRAVITY.md) for your existing Google
login, with routing scoped to the `agy` process and interactive or headless use.

### OpenCode
Set the base URL for your selected provider and keep its model/auth settings.
See [other harness integrations](HARNESS_SETUP.md#other-harnesses).

### Codex
Choose the [Codex recipe](HARNESS_SETUP.md#codex) for your ChatGPT login or
OpenAI API key. Use its HTTP/SSE settings for response plugins and usage logging.

### Aider
Use the compatible provider adapter with your chosen model and matching Torana
route. See [other harness integrations](HARNESS_SETUP.md#other-harnesses).

### OpenHands / Continue.dev
In the harness's provider settings, point the selected provider at its matching
Torana route and keep the model and authentication you normally use. An
OpenAI Chat Completions base URL uses `/provider/<name>/v1`; select a route
whose upstream serves that API. Check the resulting request in Torana's **Live Feed**.

## Verify

```bash
curl --fail-with-body "$(torana endpoint)/health"
torana stats
torana status
```

A failed plugin hot reload keeps the last known-good pipeline serving and makes
`/health` return 503 with `status: degraded` until a valid bundle reloads.

Once traffic has flowed, `torana conversations` lists what the proxy has seen:

```
ID            LAST ACTIVE  TURNS  MODEL              CACHE
a3f9c2e1      2m ago       12     claude-sonnet-4-5  118k cached
7b1e04aa      41m ago      3      gemini-2.5-pro     62k read
```

The CACHE column is the provider's own accounting, and it is the quickest way to
tell whether prompt caching is working for you: **cached** means the prefix was
served from cache, **written** means it had to be rebuilt. A conversation that
resumes after a pause and shows "written" paid full price for history it had
already sent. See [Prompt caching](PROMPT_CACHING.md).

Torana records identifiers, timestamps and token counts here — never message
content.

## Switch back

Exit the harness and launch it normally if you used command-scoped routing.
If you edited a saved harness configuration, restore its previous provider
settings. Then run `torana stop --yes` for the running instance. Use `torana serve` if you prefer foreground serving.
