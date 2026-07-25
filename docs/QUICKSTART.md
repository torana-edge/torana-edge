# Torana Edge — Quickstart

Torana Edge sits between your AI coding harness and your LLM provider. It
normalizes provider formats and runs an ordered WASM plugin pipeline. Optional
tool-aware and provider-native compaction can reduce repeated context while
preserving exact evidence according to explicit policies.

## 1-minute install

```bash
go install github.com/torana-edge/torana-edge/cmd/torana@latest
```

The command above installs the proxy, not the WASM artifacts. To use bundled
plugins, clone the repository and build them:
```bash
git clone https://github.com/torana-edge/torana-edge
cd torana-edge
make build
```

## Configure

Create `config.json`:
```json
{
  "port": 8080,
  "providers": {
    "deepseek": {
      "url": "https://api.deepseek.com/beta",
      "format": "openai"
    },
    "openai": {
      "url": "https://api.openai.com",
      "format": "openai",
      "fallback": ["deepseek"]
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

On the first run, Torana imports this seed into its managed store at
`~/.config/torana/config.json` (or `$TORANA_DATA_DIR/config.json`). After that,
the managed store is authoritative so Control Plane edits survive restarts.
Changing the original seed does not overwrite managed state; Torana logs a
warning when both files exist and differ. Edit the managed configuration through
`http://127.0.0.1:8080/_torana/`, or remove the managed store if you deliberately
want the next start to re-import the seed. `TORANA_CONFIG` selects a different
seed path; it does not bypass an existing managed store.

The empty order is intentional: discovered plugins are not implicitly trusted
or enabled. Open the Control Plane, inspect and approve the exact bundle digest
and requested capability subset, then enable plugins in the desired order.

> The baseline leaves all tool output exact. To enable compaction, approve and
> enable `intent`, then place one approved compactor after it and configure
> explicit tool policies. Unknown tools, mutations, and failures remain exact.
> See [COMPACTION.md](COMPACTION.md).

## Route your harness

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
export ANTHROPIC_AUTH_TOKEN=<your-deepseek-key>
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

### Aider
```bash
export OPENAI_API_BASE=http://localhost:8080/provider/deepseek/v1
export OPENAI_API_KEY=<your-key>
aider --model deepseek/deepseek-v4-flash
```

### OpenHands / Continue.dev
Configure the provider URL to `http://localhost:8080/provider/deepseek/v1`
and API key in the respective settings UI. Torana is compatible with any
tool that sends OpenAI-compatible chat completion requests.
