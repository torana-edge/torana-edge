# Use the harness you already have

Start Torana using the [quickstart](QUICKSTART.md), then choose your harness.
You do not need a DeepSeek account. Keep the provider you already use; your
provider’s billing and usage limits still apply.

These examples use Torana’s default local port, 8080. Change it if your
instance uses another port. Command-scoped settings affect only that launch.

## Claude Code

For an existing Claude login, keep this native provider in Torana Settings:

```json
"anthropic": {
  "url": "https://api.anthropic.com",
  "format": "anthropic",
  "auth": {"mode": "caller"}
}
```

It is already in `config.example.json`. Launch:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8080/provider/anthropic claude
```

No new key is required when Claude Code already has an active subscription
login. An existing `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, or `apiKeyHelper`
can take precedence; check your intended auth method before testing. Do not
replace subscription credentials with a different provider’s key.
[Claude’s gateway documentation](https://code.claude.com/docs/en/llm-gateway)
explains subscription and API-key behavior.

Try asking it to read a small non-sensitive file. Check the request and
tool-result follow-up in Torana’s Feed. Exit and run `claude` normally to return
to your usual route.

## Codex

Codex uses OpenAI Responses, not Chat Completions. An OpenAI API key and a
ChatGPT login also use different upstream endpoints. Keep them separate.

For an **OpenAI API key**, the example `openai` Torana provider points to
`https://api.openai.com`. A dedicated provider in `~/.codex/config.toml` is:

```toml
[model_providers.torana]
name = "Torana"
base_url = "http://127.0.0.1:8080/provider/openai/v1"
wire_api = "responses"
env_key = "OPENAI_API_KEY"
supports_websockets = false
```

Select it for one launch with `codex -c 'model_provider="torana"'`; keep your
normal model selection. This uses HTTP/SSE. The [Codex configuration
reference](https://learn.chatgpt.com/docs/config-file/config-reference) describes
provider overrides. This API-key recipe was configuration-reviewed, not tested
with a live API key in the session below.

**ChatGPT-login verification:** a dedicated native Torana provider pointing at
`https://chatgpt.com/backend-api/codex`, with caller auth, returned a Luna
read-tool round trip in Codex 0.154.0. The final check used a custom Codex
provider with `requires_openai_auth = true` and `supports_websockets = false`.
Codex request compression remained enabled. The upstream HTTP 200 omitted
`Content-Type`; Torana classified it from the already-recognized Responses
request, ran the response pipeline, recorded feed usage, and `usage_logger`
wrote a content-free record with `usage_reported: true`.

Add a separate Torana provider named `chatgpt` with URL
`https://chatgpt.com/backend-api/codex`, format `openai`, and auth mode `caller`.
Then run this command-scoped Codex provider without changing saved Codex config:

```bash
codex exec --ignore-user-config --ephemeral --skip-git-repo-check -s read-only \
  -m gpt-5.6-luna \
  -c 'model_provider="torana"' \
  -c 'model_providers.torana={name="Torana",base_url="http://127.0.0.1:8080/provider/chatgpt",wire_api="responses",requires_openai_auth=true,supports_websockets=false,request_max_retries=0,stream_max_retries=0}' \
  -c check_for_update_on_startup=false \
  'Reply with exactly: 42'
```

Keep `supports_websockets = false` for response-plugin workflows. A separate
bounded check confirmed that WebSocket transport connects through Torana, but
an upgraded connection is an opaque byte stream and does not enter the HTTP/SSE
response-plugin or usage pipeline. Do not substitute an API key for a ChatGPT
login without choosing API billing explicitly.

## Antigravity CLI (`agy`)

For the signed-in Google Code Assist path, use the [local TLS-ingress
setup](GEMINI_ANTIGRAVITY.md). It maps only the named Code Assist hosts and
forwards the harness’s own authentication. Once configured:

```bash
HTTPS_PROXY=http://127.0.0.1:8099 \
SSL_CERT_FILE=/absolute/path/to/local/mitm/bundle.pem \
agy
```

Use the actual bundle path reported by your instance. Do not install this CA
into system trust. The environment settings above apply only to this process;
your next normal `agy` launch does not use them.

## pi and oh-my-pi

Choose and sign into a provider in the harness first. Both support provider
base-URL overrides, but they use different files:

- **pi:** `~/.pi/agent/models.json`, under `providers.<provider>.baseUrl`.
- **oh-my-pi (`omp`):** `~/.omp/agent/models.yml`, under
  `providers.<provider>.baseUrl`.

For example, an Anthropic API provider uses Torana’s native Anthropic route;
an OpenAI-compatible Chat provider uses its own route with `/v1`. Preserve the
other provider/model/auth settings instead of replacing the whole file.
OAuth-backed provider extensions can have their own endpoint behavior: verify
the selected provider rather than assuming every login uses the same URL.

See [pi’s model configuration](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/models.md)
and [oh-my-pi’s provider guide](https://github.com/can1357/oh-my-pi/blob/main/docs/providers.md).
The local CLIs were inspected; credentialed pi/omp runs have not been performed.

## Live checks and what they prove

Small checks on September 16, 2026, using native routes (initially without plugins):

| Harness | Model | Observed result |
| --- | --- | --- |
| Claude Code 2.1.271 | Haiku 4.5 | Read-tool call and follow-up succeeded; matching HTTP 200 entries and usage appeared in Torana’s feed. A later request wrote usage records after enabling usage_logger through the UI |
| Codex 0.154.0 | GPT-5.6 Luna | Compressed HTTP/SSE text and read-tool turns succeeded through ChatGPT login; feed usage and a `usage_logger` record were verified. WebSocket connectivity was checked separately and is intentionally not the response-plugin path |
| Antigravity language server 1.2.2 | Gemini 3.8 Flash High | Text response succeeded through the mapped Code Assist host; HTTP 200 and usage appeared in the feed |

These are narrow checks, not claims about resume, every tool, login refresh,
or every provider account. For plugin verification, enable `usage_logger` in
the UI and confirm that another request creates a log record. Its
[own guide](https://github.com/torana-edge/torana-plugins/blob/main/plugins/usage_logger/README.md)
covers configuration and shell-native output reading.

## Other harnesses

These are documented integration paths to test, not additional live results:

- [OpenCode](https://opencode.ai/docs/providers/#base-url): provider-specific
  `options.baseURL`. Select the adapter matching the route’s API.
- [Gemini CLI](https://github.com/google-gemini/gemini-cli/blob/main/docs/reference/configuration.md):
  `GOOGLE_GEMINI_BASE_URL` for Gemini **API-key** authentication. That setting
  does not establish support for the separate Google-login Code Assist route.
- [GitHub Copilot CLI](https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/use-byok-models):
  its bring-your-own-provider configuration includes a custom base URL. This
  is separate from routing a Copilot subscription.
- [Aider](https://aider.chat/docs/llms/openai-compat.html): `OPENAI_API_BASE`
  and a matching `openai/<model>` selection for OpenAI-compatible endpoints.

For an API-key-only test without a harness, use the
[optional DeepSeek example](QUICKSTART.md#optional-use-an-api-key-directly),
adapting configuration and the request to your own provider.
