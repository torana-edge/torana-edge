# Torana Edge — Quickstart

Torana Edge sits between your AI coding harness and your LLM provider.
It intercepts tool calls, extracts intent, and compacts massive tool
results through a cheaper model — cutting token costs by 90%+ on
file reads, grep searches, and other repetitive coding tasks.

## 1-minute install

```bash
go install github.com/torana-edge/torana-edge/cmd/torana@latest
```

Or clone and build:
```bash
git clone https://github.com/torana-edge/torana-edge
cd torana-edge
go build -o torana ./cmd/torana/
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
  "offload": {
    "enabled": true,
    "model": "deepseek-v4-flash",
    "provider": "deepseek"
  }
}
```

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
