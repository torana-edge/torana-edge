# Torana Edge — Quickstart

Torana Edge sits between your AI coding harness and your LLM provider. It
normalizes provider formats and runs an ordered WASM plugin pipeline over chat
requests and responses. Optional compactors can reduce large tool outputs for
workloads where lossy reduction is acceptable.

## 1-minute install

```bash
go install github.com/torana-edge/torana-edge/cmd/torana@latest
```

This installs the proxy executable only. It does not install plugin source or
compiled `.wasm` artifacts. Use it for provider routing without plugins, or
provide a separately built plugin directory.

To use the bundled plugins, clone the repository and build them:
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
      "url": "https://api.openai.com/v1",
      "format": "openai",
      "fallback": ["deepseek"]
    }
  },
  "plugins": {
    "dir": "./plugins",
    "order": ["schema_translator", "intent"]
  },
  "limits": {
    "concurrency": 10,
    "rpm": 100
  }
}
```

> **Coding-agent safety:** the current compactors are lossy and may alter a
> fresh file-read result before the model sees it, breaking exact-match edits.
> The configuration above deliberately enables no compactor while
> [#166](https://github.com/torana-edge/torana-edge/issues/166) is open. For a
> research-only workload, append **one** compactor after `intent`:
> `keyword_compactor` (deterministic/local) or `compactor` (model offload),
> never both.

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
