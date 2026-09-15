# Coding-harness compatibility

Pointing a coding harness at Torana changes only the provider inference traffic
that Torana explicitly understands. On a native route (no `bridge` configured),
account, quota, status, telemetry, update, model-list, MCP, and unknown auxiliary
requests remain ordinary reverse-proxy traffic and never enter the inference
plugin pipeline. Any configured protocol bridge instead refuses auxiliary APIs;
see [Protocol bridges](#protocol-bridges).

This is an endpoint contract, not a list of blessed clients. Claude Code,
Codex, OpenCode, Aider, oh-my-pi, or a new client using the same provider
endpoint gets the same behavior.

## Inference boundary

Only these requests are decoded to Torana's IR and exposed to plugins:

| Provider format | Inference requests |
|---|---|
| OpenAI | `POST .../chat/completions`, `POST .../responses` |
| Anthropic | `POST .../messages` |
| Gemini / Code Assist | `POST ...:generateContent`, `POST ...:streamGenerateContent` |

Method and endpoint position matter. A look-alike path, a `GET`, or a path
where the token is not the final operation does not enter IR.

## What pass-through guarantees

For a native provider route, non-inference traffic keeps its method, upstream
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

Use the harness's supported base-URL setting whenever it has one. These are
native integration paths; for a bridge, select the explicit client contract
and verify the additional boundaries below:

| Harness | Setup example | Connection |
| --- | --- | --- |
| Claude Code | [Quickstart](QUICKSTART.md#claude-code) | `ANTHROPIC_BASE_URL` with an Anthropic-format route |
| Codex | [Quickstart](QUICKSTART.md#codex) | Custom OpenAI Responses provider |
| OpenCode | [Quickstart](QUICKSTART.md#opencode) | Selected provider's base URL |
| Aider | [Quickstart](QUICKSTART.md#aider) | OpenAI-compatible base URL |
| oh-my-pi | [Quickstart](QUICKSTART.md#omp-oh-my-pi) | Selected provider's base URL |
| Antigravity | [Quickstart](QUICKSTART.md#antigravity-cli-agy) | Optional TLS ingress for Code Assist |
| OpenHands / Continue.dev | [Quickstart](QUICKSTART.md#openhands--continuedev) | Selected provider's base URL |

These are configuration examples, not proof that every live client/backend
combination works. Verify your own endpoint and workflow as described below.

## Protocol bridges

A provider can explicitly accept one inference contract and call an upstream
that serves another. The contracts are `openai-chat`, `openai-responses`,
`anthropic`, `gemini`, and `gemini-codeassist`. `bridge.client` names what the
harness sends; `bridge.upstream` names what the backend accepts. Provider
`format` describes the upstream family, not the client API. No plugin is needed.
See the [bridge guide](PROTOCOL_BRIDGES.md) for the supported translation surface
and the [CLI workflow](CLI.md#protocol-bridges) for live configuration.

For example, an Anthropic Messages client can send requests through a bridge
to a local OpenAI Chat Completions backend. Cross-family bridges require
explicit upstream authentication (`credential` or `none`); caller credentials
are not forwarded across families. The backend model still needs to support
the requested tools, images, and output constraints. The harness executes tools.

The mock-HTTP suite covers all 20 cross-contract directions for common text and
function-tool loops, JSON responses, and SSE streams. This is not a claim that
every live harness/backend combination has been tested. Clients must supply
complete history; provider-native server tools, signed reasoning, cache
breakpoints, opaque continuation items, and `previous_response_id` are not
portable. Unsupported request semantics return 400 before an upstream call;
non-portable response content produces a 502 or terminates an already-started
stream without a success marker.

Under any configured bridge, model discovery, token-counting, files,
batch APIs, response retrieval, and other auxiliary APIs are not emulated or
forwarded; they return 400. This includes bridges whose client and upstream
contracts match, such as a same-API route using `bridge.model` for aliasing.
Matching contracts avoids cross-API content conversion, not the inference-only
endpoint boundary. A harness requiring one of these APIs needs a
compatible route or additional contract support. Do not infer complete harness
compatibility from a successful inference request or base-URL configuration.

## Provider behavior Torana preserves

These preservation guarantees describe native routes. Do not apply them to
cross-contract translation; use the bridge surface above instead.

- Anthropic string and array system prompts, structured tool results,
  cache-control markers, signed content, and streaming topology.
- OpenAI Chat Completions and Responses as distinct wire layouts. Responses
  function tools and provider-native free-form/custom tools are typed in the
  IR; unmodelled Responses items remain opaque and retain their raw payload.
- Gemini and Code Assist wrapper data, part metadata, thought signatures,
  cache facts, and stream framing.
- Caller compression negotiation without allowing compressed inference
  responses to bypass response hooks.
- Provider-native prompt-cache semantics documented in
  [Prompt caching](PROMPT_CACHING.md).

## Verify your harness

The endpoint and wire-shape contracts above are covered in CI. They do not
claim that every release of every third-party harness has been manually tested.
For your setup, check a normal turn, a streamed tool turn, resume, model
discovery, and representative account/status traffic. For a bridge, record the
client contract, upstream contract, model, and harness version separately. Check
a complete tool-result follow-up and the auxiliary APIs that client actually
requires. A required endpoint returning the documented 400 is not a successful
harness smoke test.

Harness-local features such as file editing, MCP execution, and approvals remain
the harness's responsibility. Verify login refresh, telemetry preferences, and
updates separately; native pass-through is not a bridge guarantee.

If a harness adds a new inference endpoint, a native route initially passes it
through without inference hooks; any configured bridge refuses it. Support should
be added only with an explicit format adapter and positive and negative endpoint
tests—never by broad substring matching.
