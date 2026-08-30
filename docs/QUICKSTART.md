# Torana Edge — Quickstart

Torana Edge sits between your AI coding harness and your LLM provider. It
normalizes provider formats and runs an ordered WASM plugin pipeline. Optional
tool-aware and provider-native compaction can reduce repeated context while
preserving exact evidence according to explicit policies.

## Prerequisites

You need Git, Go 1.26.6 or newer, and a credential for at least one provider.
The commands below use DeepSeek, but the routing model is the same for every
configured provider.

## Install the current pre-release

```bash
git clone https://github.com/torana-edge/torana-edge.git
cd torana-edge
go build -o ./torana ./cmd/torana
cp config.example.json config.json
```

Torana Edge is intentionally unversioned. For a reproducible deployment, replace
`main` with a reviewed commit SHA before building. No WASM plugins are bundled.
The supplied configuration is deliberately minimal and enables none.

First prove the proxy works without plugins. The example providers use caller
authentication, so the harness or curl supplies the provider credential on each
request. For a disposable evaluation, keep managed state in the checkout:

```bash
export DEEPSEEK_API_KEY='replace-with-your-deepseek-key'
export TORANA_DATA_DIR="$PWD/.torana-data"
./torana --debug
```

Keep that terminal open. In another terminal:

```bash
curl --fail-with-body http://127.0.0.1:8080/health

curl --fail-with-body http://127.0.0.1:8080/provider/deepseek/v1/chat/completions \
  -H "Authorization: Bearer ${DEEPSEEK_API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"Reply with exactly: Torana works"}]}'
```

The health endpoint returns `{"status":"ok"}` and the second command returns a
normal provider response. The debug terminal prints safe request-received and
request-completed lines, so you can verify the traffic really crossed Torana;
it never logs headers or bodies.

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
or enabled. After the plugin-free request above succeeds, install one plugin:

```bash
./torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/usage_logger
./torana plugin list
```

Open the Control Plane, inspect and approve the exact bundle digest and requested
capability subset and private-file budget, then enable `usage_logger` and put it
in the pipeline. Send a few requests and inspect its content-free output:

```bash
./torana plugin file tail usage_logger usage.jsonl
```

Installation alone never grants or enables anything, and the plugin cannot pick
an OS path: Torana owns the private rotating file.

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

Every provider has one authentication mode:

- `caller` (the default when omitted) forwards the immutable credential
  captured when that request entered Torana;
- `credential` resolves a named Torana credential and injects it host-side;
- `none` sends no credential, useful for a local model.

Configure a reusable environment-backed credential without putting its value in
JSON:

```bash
./torana credential set fallback-api-key --env FALLBACK_API_KEY
```

Credential commands update durable configuration. If Torana is already
running, restart it after adding, changing, or deleting a credential so the
new provider registry and local encrypted store are loaded together.

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

Torana only sends explicitly supported inference endpoints through its IR and
plugins. Model-list, account, quota, status, telemetry, update, MCP, and unknown
auxiliary requests pass through normally. See the precise per-format contract
in [Coding-harness compatibility](HARNESS_COMPATIBILITY.md).

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
        "baseURL": "http://localhost:8080/provider/deepseek"
      }
    }
  }
}
```

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

### Aider
```bash
export OPENAI_API_BASE=http://localhost:8080/provider/deepseek/v1
export OPENAI_API_KEY='replace-with-your-key'
aider --model deepseek/deepseek-v4-flash
```

### OpenHands / Continue.dev
Configure the provider URL to `http://localhost:8080/provider/deepseek/v1`
and API key in the respective settings UI. Torana is compatible with any
tool that sends OpenAI-compatible chat completion requests.
