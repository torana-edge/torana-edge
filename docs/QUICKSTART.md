# Torana Edge — Quickstart

Get a request flowing through Torana, see it in the feed, then add one plugin.
Torana sits between your coding harness and model provider so your plugins can
work across supported APIs.

## Prerequisites

You need Git and Go 1.26.6 or newer. Which coding harness do you already use?
Start with its existing provider/login using the [harness setup guide](HARNESS_SETUP.md).
A separate API key is only needed if you choose the direct API-key example.

## Install from source

```bash
git clone https://github.com/torana-edge/torana-edge.git
cd torana-edge
go build -o ./torana ./cmd/torana
cp config.example.json config.json
```

The available install path is a source build. For a reproducible deployment,
check out a reviewed commit SHA before building. No WASM plugins are bundled;
the supplied configuration enables none.

First prove the proxy works without plugins. The example providers use caller
authentication, so the harness or curl supplies the provider credential on each
request. For a disposable evaluation, keep managed state in the checkout:

```bash
export TORANA_DATA_DIR="$PWD/.torana-data"
./torana --debug start
./torana status
```

The repository ignores this disposable directory. It still contains the
authoritative managed config, encrypted credentials, durable plugin state, and
private plugin files, so delete it when the evaluation is over and do not copy
it into source control elsewhere. This Torana process receives no provider
credential at startup. The process runs in the background.

## Connect your harness

For an already signed-in Claude Code installation:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8080/provider/anthropic claude
```

Ask it to read a small non-sensitive file, then open the local control plane’s
Feed at `http://127.0.0.1:8080/_torana/`. Keep the `anthropic` provider’s
authentication set to **Use harness credentials**; no DeepSeek key is needed.
For Codex, Antigravity, pi, or oh-my-pi, use the
[harness-specific settings and verification results](HARNESS_SETUP.md).

You can also inspect activity from the terminal:

```bash
./torana feed
```

## Optional: use an API key directly

Already have an API key and want to make a request without a harness?
Here is a DeepSeek example. For another provider, modify its URL, format and
authentication in Settings, then adapt the request endpoint, model, and payload
to that provider’s API. Choose a model available to your account.

```bash
export DEEPSEEK_API_KEY='replace-with-your-deepseek-key'

curl --fail-with-body http://127.0.0.1:8080/health

curl --fail-with-body http://127.0.0.1:8080/provider/deepseek/v1/chat/completions \
  -H "Authorization: Bearer ${DEEPSEEK_API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-flash","messages":[{"role":"user","content":"Reply with exactly: Torana works"}]}'
```

The health endpoint returns `{"status":"ok"}` and the second command returns a
normal provider response. Run `./torana feed` to find the matching request;
`./torana status` reports the log path. Debug logging records safe
request-received/completed lines, not headers or bodies. Bridge error-body
diagnostics have a separate [explicit opt-in](PROTOCOL_BRIDGES.md#diagnose-an-upstream-rejection).

## Configure

The copied [config.example.json](../config.example.json) is the complete seed:
it includes native Anthropic, OpenAI, Gemini, and DeepSeek routes, with no
plugins enabled. Choose the route for your harness; you do not need an account
with every listed provider. For a ChatGPT login or Antigravity, follow the
[harness guide](HARNESS_SETUP.md) for the appropriate endpoint and setup.

`limits.concurrency` is the maximum number of simultaneous upstream requests
per identity; `limits.rpm` is a per-identity token bucket refilled over one
minute. Set either value to `0` to disable that limit. Standard JSON does not
support comments, so the shipped file stays directly parseable; these field
descriptions and the hints in the local Control Plane are the annotated
reference.

On the first run, Torana imports this seed into its managed store at
`$TORANA_DATA_DIR/config.json`, or the platform's
[user-config directory](CLI.md#environment-variables) when unset. After that,
the managed store is authoritative so Control Plane edits survive restarts.
Changing the original seed does not overwrite managed state; Torana logs a
warning when both files exist and differ. Edit the managed configuration through
`http://127.0.0.1:8080/_torana/`, or remove the managed store if you deliberately
want the next start to re-import the seed. `TORANA_CONFIG` selects a different
seed path; it does not bypass an existing managed store.

For repeated first-run testing, point `TORANA_DATA_DIR` at a new empty directory
for each run. That avoids accidentally exercising an older managed
configuration while believing you are testing a changed seed.

## Add one plugin

The empty plugin order is intentional: discovered plugins are not implicitly
trusted or enabled. After the plugin-free request above succeeds, leave Torana
running and choose one PII guard. The watcher discovers the new bundle without
a restart.

### Recommended: use your local model

If you already run Ollama or another OpenAI-compatible local model endpoint,
install the contextual guard:

```bash
./torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/pii
./torana plugin list
```

In the local control plane, add a provider such as `local-scanner` with the URL
of your existing local server, format **OpenAI**, and authentication **None**.
Select **pii**, keep its default fail-closed settings, and bind the required
`scanner` service to `local-scanner`, the model you loaded, and
`/v1/chat/completions`. Review the digest, permissions and model-call limits,
then choose **Approve and enable**. Eligible tool output goes to that scanner;
using a remote scanner would send it to that remote endpoint.

The [model-backed PII guide](https://github.com/torana-edge/torana-plugins/blob/main/plugins/pii/README.md)
includes the exact CLI configuration and approval document.

### No local model? Use deterministic checks

Install the zero-model guard instead:

```bash
./torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/pii_guard
```

Select **pii_guard** in the control plane, review its two permissions, then
choose **Approve and enable**. It makes no model or network calls. The
[deterministic guard guide](https://github.com/torana-edge/torana-plugins/blob/main/plugins/pii_guard/README.md)
also covers configuration through the CLI.

Install only one guard; their manifests declare the pair as conflicting. Now
create an obviously synthetic credential:

```bash
printf '%s\n' 'PAYMENT_API_KEY=sk_test_torana_demo_not_a_real_key_123' > .keys
```

Ask your routed harness to read `.keys`. Either guard should return a
value-free `sensitive_data_detected` block before the tool result reaches the
primary provider. Obvious patterns take the deterministic fast path in both
plugins; the model-backed option also checks eligible ambiguous content with
your bound local scanner. Remove `.keys` after the walkthrough.

Installation alone never approves, enables, or runs anything. The installer
also accepts a reviewed local plugin directory or another repository URL.

Software agents and shell scripts can discover the same guarded control-plane
capabilities as JSON:

```bash
curl --fail-with-body --silent \
  http://127.0.0.1:8080/_torana/api/v1/ | jq
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
./torana stop --yes
./torana credential set fallback-api-key --env FALLBACK_API_KEY
# Export FALLBACK_API_KEY in this shell before starting the host.
./torana start
./torana status
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
whose upstream serves that API. Check the resulting request in Torana's Feed.

## Verify

```bash
curl --fail-with-body http://127.0.0.1:8080/health
./torana stats
./torana status
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
settings. Then run `./torana stop --yes` in the shell with the same
`TORANA_DATA_DIR`. Use `./torana serve` if you prefer foreground serving.
