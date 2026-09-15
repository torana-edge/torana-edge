# Torana Edge — Quickstart

Get a request flowing through Torana, see it in the feed, then add one plugin.
Torana sits between your coding harness and model provider so your plugins can
work across supported APIs.

## Prerequisites

You need Git, Go 1.26.6 or newer, and a credential for at least one provider.
The commands below use DeepSeek, but the routing model is the same for every
configured provider.

## Install from source

<!-- torana:source-install:start -->
```bash
git clone https://github.com/torana-edge/torana-edge.git
cd torana-edge
go build -o ./torana ./cmd/torana
cp config.example.json config.json
```
<!-- torana:source-install:end -->

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
credential. The process runs in the background. In the same shell, configure
the caller credential and send the request:

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

Use `./torana stop --yes` when finished. `./torana serve` is available if
you prefer a foreground process.

## Configure

The copied `config.example.json` is equivalent to this minimal seed:
```json
{
  "port": 8080,
  "providers": {
    "deepseek": {
      "url": "https://api.deepseek.com",
      "format": "openai",
      "auth": {"mode": "caller"}
    },
    "deepseek-anthropic": {
      "url": "https://api.deepseek.com/anthropic",
      "format": "anthropic",
      "auth": {"mode": "caller"}
    }
  },
  "plugins": {
    "dir": "./plugins",
    "order": []
  },
  "limits": {
    "concurrency": 10,
    "rpm": 100
  }
}
```

`limits.concurrency` is the maximum number of simultaneous upstream requests
per identity; `limits.rpm` is a per-identity token bucket refilled over one
minute. Set either value to `0` to disable that limit. Standard JSON does not
support comments, so the shipped file stays directly parseable; these field
descriptions and the hints in the local Control Plane are the annotated
reference.

On the first run, Torana imports this seed into its managed store at
`~/.config/torana/config.json` (or `$TORANA_DATA_DIR/config.json`). After that,
the managed store is authoritative so Control Plane edits survive restarts.
Changing the original seed does not overwrite managed state; Torana logs a
warning when both files exist and differ. Edit the managed configuration through
`http://127.0.0.1:8080/_torana/`, or remove the managed store if you deliberately
want the next start to re-import the seed. `TORANA_CONFIG` selects a different
seed path; it does not bypass an existing managed store.

For repeated first-run testing, point `TORANA_DATA_DIR` at a new empty directory
for each run. That avoids accidentally exercising an older managed
configuration while believing you are testing a changed seed.

The empty order is intentional: discovered plugins are not implicitly trusted
or enabled. After the plugin-free request above succeeds, leave Torana running
and install one plugin from the same checkout. The watcher discovers the new
bundle without a restart:

```bash
./torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/usage_logger
./torana plugin list
```

The installer also accepts a local directory. This is useful for a private
repository that you clone and review yourself:

```bash
./torana plugin install ../my-private-plugins/usage_logger
```

Run `./torana plugin inspect usage_logger`, then follow its
[complete CLI approval example](https://github.com/torana-edge/torana-plugins/blob/main/plugins/usage_logger/README.md).
Approve the exact bundle digest, full requested permission set and private-file
budget, then enable it. The local UI offers the same steps. Send a few requests
and inspect its content-free output:

```bash
tail -F "$(./torana plugin file path usage_logger usage.jsonl)"
```

Installation alone never approves, enables, or runs anything, and the plugin
cannot pick an OS path: Torana owns the private rotating file. The operator can
resolve that local path for standard Unix tools; `tail -F` continues following
it when Torana rotates the file. The running instance is authoritative for its
data directory, so a fresh second terminal needs no repeated environment setup.

Once that lifecycle is clear, the maintained set can be built locally with:

```bash
./torana plugin install --official
```

It clones the official source repository and builds each selected plugin. The
additional bundles also remain disabled until individually approved and ordered.

Software agents and shell scripts can discover the same guarded control-plane
capabilities as JSON:

```bash
curl --fail-with-body --silent \
  http://127.0.0.1:8080/_torana/api/v1/ | jq
```

See [AGENT_CONTROL_PLANE.md](AGENT_CONTROL_PLANE.md) for stable operation IDs,
JSON error envelopes, mutation guidance, and plugin-contributed operations.

> The baseline leaves all tool output exact. To enable compaction, approve one
> compactor and configure explicit tool policies. `intent` is optional; placing
> it before the compactor improves relevance, while a bounded local signal is
> used when it is absent. Unknown tools, mutations, and failures remain exact.
> See [COMPACTION.md](COMPACTION.md).


### Provider authentication and fallbacks

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
plugins. Native routes forward auxiliary calls as ordinary HTTP; an explicit
bridge rejects them with HTTP 400, even when both bridge contracts are the
same. See [Coding-harness compatibility](HARNESS_COMPATIBILITY.md) and
[Protocol bridges](PROTOCOL_BRIDGES.md). The following auxiliary check uses the
native DeepSeek route from this guide.

You can verify the auxiliary-path half of that contract directly:

```bash
curl -i http://127.0.0.1:8080/provider/deepseek/models
```

The provider may return success or its own error for that endpoint. The important
property is that it is forwarded as ordinary HTTP and does not appear in
Torana's live inference feed.

### omp (oh-my-pi)
```yaml
# ~/.omp/agent/models.yml
providers:
  deepseek:
    baseUrl: http://localhost:8080/provider/deepseek/v1
```

### Claude Code
```bash
export ANTHROPIC_BASE_URL=http://localhost:8080/provider/deepseek-anthropic
export ANTHROPIC_AUTH_TOKEN='replace-with-your-deepseek-key'
```

### Antigravity CLI (agy)
`agy` can't take a base URL, so route it through Torana's MITM ingress — see
[GEMINI_ANTIGRAVITY.md](GEMINI_ANTIGRAVITY.md):
```bash
export HTTPS_PROXY=http://127.0.0.1:8099
export SSL_CERT_FILE=/abs/path/to/local/mitm/bundle.pem
```

### OpenCode
```jsonc
// ~/.config/opencode/opencode.jsonc
{
  "provider": {
    "deepseek": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "http://localhost:8080/provider/deepseek/v1"
      }
    }
  }
}
```

Follow [OpenCode's provider configuration](https://opencode.ai/docs/providers/)
to connect your credential and select a model. The compatible adapter's base URL
includes `/v1`; verify the resulting request in Torana's feed.

### Codex
Codex reaches a custom provider through `~/.codex/config.toml`, and it speaks
the OpenAI **Responses** API rather than Chat Completions — so point it at an
`openai`-format Torana provider whose upstream serves `/responses`:

```toml
model = "gpt-5.2-codex"
model_provider = "torana"

[model_providers.torana]
name = "Torana"
base_url = "http://localhost:8080/provider/openai/v1"
env_key = "OPENAI_API_KEY"
wire_api = "responses"
```

This example uses a dedicated `torana` provider entry so its base URL is
separate from your other provider settings.

### Aider
```bash
export OPENAI_API_BASE=http://localhost:8080/provider/deepseek/v1
export OPENAI_API_KEY='replace-with-your-key'
aider --model openai/deepseek-flash
```

This follows Aider's [OpenAI-compatible configuration](https://aider.chat/docs/llms/openai-compat.html):
the `openai/` model prefix selects the adapter that uses these variables.
Confirm the request appears in `./torana feed`.

### OpenHands / Continue.dev
Configure the provider URL to `http://localhost:8080/provider/deepseek/v1`
and API key in the respective settings UI. Torana is compatible with any
tool that sends OpenAI-compatible chat completion requests.

## Verify

```bash
curl http://localhost:8080/health   # {"status":"ok"}
curl http://localhost:8080/stats    # compaction counters
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
