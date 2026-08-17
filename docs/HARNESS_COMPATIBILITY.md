# Coding-harness compatibility

Pointing a coding harness at Torana changes only the provider inference traffic
that Torana explicitly understands. Account, quota, status, telemetry, update,
model-list, MCP, and unknown auxiliary requests remain ordinary reverse-proxy
traffic and never enter the inference plugin pipeline.

This is an endpoint contract, not a list of blessed clients. Claude Code,
Codex, OpenCode, Aider, or a new client using the same provider endpoint gets
the same behavior.

## Inference boundary

Only these requests are decoded to Torana's IR and exposed to plugins:

| Provider format | Inference requests |
|---|---|
| OpenAI | `POST .../chat/completions`, `POST .../responses` |
| Anthropic | `POST .../messages` |
| AWS Bedrock | `POST .../converse`, `POST .../converse-stream` |
| Gemini / Code Assist | `POST ...:generateContent`, `POST ...:streamGenerateContent` |

Method and endpoint position matter. A look-alike path, a `GET`, or a path
where the token is not the final operation does not enter IR.

Bedrock's model-specific Invoke API is deliberately pass-through. Torana owns
the Bedrock Converse wire format; treating every endpoint whose name sounds
like inference as Converse would corrupt valid model-specific traffic.

## What pass-through guarantees

For a configured provider, non-inference traffic keeps its method, upstream
path, query, body, and upstream response. Neither request nor response WASM
hooks run. Transport security policy can still remove credentials that must
not cross a provider boundary; Torana does not promise to relay arbitrary
secrets to a different fallback provider.

The boundary is executable. The proxy tests send non-JSON account traffic and
empty-body model discovery through real request and response trap plugins and
prove that the upstream bytes and status survive while both hooks remain
untouched. An empty request to a recognized inference endpoint instead fails
closed with the provider's invalid-request response.

## Harness configuration

Use the harness's supported base-URL setting whenever it has one:

- Claude Code: set `ANTHROPIC_BASE_URL` to an Anthropic-format Torana provider.
- Codex: configure an OpenAI Responses-compatible custom provider whose base
  URL points at Torana.
- OpenCode and Aider: set the selected provider's OpenAI-compatible or
  Anthropic base URL to Torana.
- Code Assist clients without a base-URL setting: use Torana's optional MITM
  ingress as described in [Gemini / Antigravity](GEMINI_ANTIGRAVITY.md).

Concrete examples are in the [quickstart](QUICKSTART.md).

## Provider behavior Torana preserves

- Anthropic string and array system prompts, structured tool results,
  cache-control markers, signed content, and streaming topology.
- OpenAI Chat Completions and Responses as distinct wire layouts, including
  opaque Responses input items.
- Gemini and Code Assist wrapper data, part metadata, thought signatures,
  cache facts, and stream framing.
- Caller compression negotiation without allowing compressed inference
  responses to bypass response hooks.
- Provider-native prompt-cache semantics documented in
  [Prompt caching](PROMPT_CACHING.md).

## Honest release boundary

The endpoint and wire-shape contracts above are covered in CI. They do not
claim that every release of every third-party harness has been manually tested.
Before an Edge release, the owner still runs credentialed smoke tests for the
clients being advertised: a normal turn, a streamed tool turn, resume, model
discovery, and representative account/status traffic. Harness-local features
such as file editing, MCP execution, approvals, login refresh, telemetry
preferences, and updates must remain local or pass through unchanged.

If a harness adds a new inference endpoint, Torana initially passes it through.
Support should be added only with an explicit format adapter and positive and
negative endpoint tests—never by broad substring matching.
