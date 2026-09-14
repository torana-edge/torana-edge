# Use a different API contract from your model backend

A Torana provider can accept one inference API and call a backend that serves
another. For example, a client can send Anthropic Messages while its local
model server receives OpenAI Chat Completions. Torana converts the request,
returns the client's JSON or SSE response, and preserves function-call IDs
for the next tool-result turn. No plugin is required.

A bridge translates the supported features below. The model still needs to
support the requested tools, image inputs, and output constraints. A bridge
does not supply missing model capabilities or execute tools for the client.

## Example: Anthropic client to a local OpenAI-compatible server

Add this provider to a first-run configuration, or submit it through the
managed configuration API on an existing installation:

```json
{
  "port": 8080,
  "providers": {
    "local-messages": {
      "url": "http://127.0.0.1:8000",
      "format": "openai",
      "auth": {"mode": "none"},
      "bridge": {
        "client": "anthropic",
        "upstream": "openai-chat",
        "model": "your-loaded-model"
      }
    }
  }
}
```

Replace `your-loaded-model` with the model name your server accepts. Send an
Anthropic request to Torana:

```sh
curl --fail-with-body http://127.0.0.1:8080/provider/local-messages/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"model":"client-model","max_tokens":128,"messages":[{"role":"user","content":"Say hello"}]}'
```

The backend receives `/v1/chat/completions` with `model: your-loaded-model`.
The client receives an Anthropic message. Set `stream: true` for Anthropic SSE.
An Anthropic-compatible client's base URL is
`http://127.0.0.1:8080/provider/local-messages`.

Use `bridge.client: openai-responses` to expose the same backend at the
Responses `/v1/responses` endpoint instead. Create separate provider names to
expose several client contracts for one server. A client must send complete
conversation history on each turn; server-managed response chains are not
emulated.

The managed store is authoritative after first start. Editing a seed file does
not change a running installation. See [Quickstart](QUICKSTART.md#configure)
and [the configuration API](AGENT_CONTROL_PLANE.md). From the terminal, use
`torana config get` and `torana config apply`; the [CLI bridge workflow](CLI.md#protocol-bridges)
covers creation, updates, removal, and background diagnostics.

## Configuration

| Field | Meaning |
|---|---|
| `format` | Upstream adapter family; must match `bridge.upstream` |
| `bridge.client` | API contract spoken by this route's client |
| `bridge.upstream` | API contract actually served by the backend |
| `bridge.model` | Optional upstream model alias, applied before request plugins |
| `bridge.max_tokens` | Optional explicit default when an Anthropic upstream needs a limit the client omitted |
| `bridge.project` | Required project for a translated Code Assist upstream |

| Protocol | Family | Inference endpoint |
|---|---|---|
| `openai-chat` | `openai` | `/v1/chat/completions` |
| `openai-responses` | `openai` | `/v1/responses` |
| `anthropic` | `anthropic` | `/v1/messages` |
| `gemini` | `gemini` | `/v1beta/models/{model}:generateContent` or `:streamGenerateContent` |
| `gemini-codeassist` | `gemini-codeassist` | `/v1internal:generateContent` or `:streamGenerateContent` |

Use an upstream origin, optionally followed by a deployment prefix, as `url`.
Torana appends the complete endpoint in the table; do not repeat `/v1` in that
base URL. For public Gemini the model comes from the client URL unless
`bridge.model` overrides it. Streaming Gemini requests use `alt=sse` upstream.

Use `auth.mode: credential` and a named [Torana credential](CREDENTIALS.md) for
an authenticated backend, or `none` for an unauthenticated local server.
Cross-family bridges require one of these explicit policies. Caller mode is
available when both contracts use the same adapter family. Client credentials,
provider beta/version headers, cookies, and query parameters are not copied
across families. Torana builds the destination headers, including
`anthropic-version: 2023-06-01` for Anthropic upstreams.

## Supported translation surface

- Text conversations, leading system instructions, token limits, temperature,
  and top-p. Leading system messages are combined for preamble-only APIs;
  mid-conversation system messages and distinct developer roles are refused
  when the destination cannot represent them.
- Ordinary JSON function definitions, named/automatic/required/disabled tool
  selection, parallel calls where representable, and complete tool-result
  follow-up turns. Argument and schema number lexemes retain their precision.
- Inline PNG, JPEG, GIF and WebP inputs; HTTP(S) image URLs between OpenAI and
  Anthropic. Remote images are not fetched or uploaded by Torana. Image-detail
  requirements and multimodal tool results need a supported mapping.
- JSON object/schema output constraints where the destination supports their
  exact semantics. Explicit strict guarantees are never weakened.
- Complete JSON replies and SSE text/function calls, response identity,
  stop/tool/length outcomes, terminal usage, HTTP error envelopes, and client
  cancellation. A truncated upstream stream does not become a successful
  completion.

Public Gemini function schemas use `parametersJsonSchema` on translated
requests. Incoming Gemini `parameters` are normalized from the supported
Schema subset into JSON Schema. Code Assist uses the supported Schema subset;
constraints with no equivalent return a capability error.

Provider-specific reasoning/signatures, free-form and namespaced tools,
built-in server tools, cache breakpoints, opaque continuation items,
`previous_response_id`, response storage/background execution, named chat
participants, and provider-specific request extensions are not portable in
this bridge. They produce a client-shaped 400 before any upstream call.
Non-portable response content produces a 502 or terminates an already-started
stream without a success marker. Ordinary documented upstream response
metadata is interpreted at the upstream boundary; it is not copied into an
unrelated client's response envelope.

A native provider route without `bridge` continues to preserve its supported
provider-specific features and original request bytes. These translation
limits do not reduce the native adapters' supported surface.

## Plugins, routing and accounting

Request plugins see canonical client content and the configured upstream model
alias. The original-request host call retains the client's original model and
content. Responses instructions and generation parameters are canonical fields,
so the same plugin helpers can inspect or modify them across APIs. Responses
instruction placement remains host-owned topology.
An approved request plugin with `ir.params.write` may change `Stream`; the
upstream endpoint and client response mode follow that accepted value, as on
native routes. The original-request snapshot still retains the caller's mode.

Response hooks operate on the actual upstream's canonical response before
client serialization. Native source usage drives host accounting. Client usage
is converted between Anthropic's uncached-input counts and OpenAI/Gemini's
inclusive-input counts. An OpenAI Chat usage opt-in added for host accounting
is shown to the client only if the client requested usage.

For example, Anthropic reporting 10 uncached input tokens and 20 cache-read
tokens remains 10/20 in host accounting, while an OpenAI client receives 30
total prompt tokens with 20 cached. Cache-write counts can be represented in
Anthropic and Responses usage; a nonzero write count is refused for Chat
Completions and Gemini, which have no matching accounting field.
An Anthropic client also requires reported usage from its upstream; omitted
usage produces a translation error rather than invented zero-token totals.

A routing plugin can select another configured bridge target with the same
`bridge.client` contract. Unsupported routing verdicts retain the original
route. On retryable upstream failures,
configured fallback bridges rebuild the request for their own protocol, model
and credentials. Plugins run once. A fallback that cannot represent the
approved request is skipped; it does not receive a weakened request. Economic
compaction decisions remain conservative across incompatible or unpriced
fallbacks, and a fallback model alias needs its own price entry.

Plugin `send_request` calls use that provider's client endpoint and canonical
request content too. Torana translates the outbound request and the returned
JSON body; spend and returned usage counters retain the upstream's native
accounting. Bridged streaming is not supported by this buffered host call.
Model-service calls retain their explicitly bound model and return canonical
results from the actual upstream. They do not require an intermediate client
response format. Plugin block/respond verdicts remain local and make no
upstream call.

Bridges own inference endpoints only. Model discovery, token-count endpoints,
files, batch APIs, response retrieval and other auxiliary APIs are not emulated
or forwarded under any configured bridge. Such calls return an explicit 400,
including when `bridge.client` equals `bridge.upstream` (for example, when using
`bridge.model` to alias a model without changing APIs). Matching contracts avoids
cross-API content conversion, not the inference-only endpoint boundary. Only a
native route without `bridge` retains ordinary auxiliary pass-through.
A harness that requires one of those APIs or provider-native tools needs
additional contract support; configuring its base URL alone is not proof of
full harness compatibility.

## Diagnose an upstream rejection

Client error responses retain the upstream HTTP status and a generic,
client-shaped message. Audit records identify these as `bridge_upstream_error`.
With `--debug`, a diagnostic records the request ID, provider, source and client
protocols, and status without logging response bodies.

For details such as an upstream model-name or token-limit rejection, explicitly
enable error-body diagnostics:

```sh
TORANA_DEBUG_UPSTREAM_ERRORS=1 ./torana --debug
```

This logs up to 8 KiB of each bridged upstream error body, with control characters
escaped and truncation/read-failure flags. It also covers translated plugin
`send_request` errors. Upstream errors can echo prompts or credentials, so this
opt-in produces sensitive local logs; disable it after diagnosis. Bodies remain
absent from client errors and audit records. Torana does not log response headers
or upstream URLs in these diagnostics.

## Implementation and verification

`internal/bridge` separates protocol identity, request projection, complete
response conversion, and guarded streaming. Existing format adapters supply
native parsing and serialization; provider transport builds paths and
credentials separately. The bridge uses the existing ABI v1 and SDK v0.5.0.

Tests exercise every cross-protocol pair through mock HTTP upstreams, tool
follow-up turns, unsupported semantics, errors, routing/fallbacks, and native
regressions. These are deterministic contract tests, not evidence of a live
account or every harness/backend combination having been tested.

CI also exports JSON and text/tool SSE fixtures for the pinned OpenAI and
Anthropic SDKs. The [client validation script](../scripts/validate-bridge-sdk.py)
checks required wire fields and exercises the SDK stream accumulators with
mock transports, including interleaved tool arguments and terminal usage.
CI installs the complete transitive dependency lock with `--require-hashes` and
`--only-binary=:all:`. To update it, edit
`scripts/bridge-sdk-validation-requirements.in` and run the `uv pip compile`
command recorded at the top of the generated `.txt` file (Python 3.10 or later).

See [upgrade notes](UPGRADE_NOTES.md) for the canonical Responses plugin fields
and the one-time cache-prefix identity change.

The [research notes](design/PROTOCOL_BRIDGE_RESEARCH.md) record pinned source
and tests from Bifrost, CLIProxyAPI and LiteLLM. They informed stream state,
identity and usage handling, and also exposed behavior we deliberately reject:
dropped tools, orphan results converted to user text, and failed streams mapped
to successful completion. No third-party runtime dependency was added.
