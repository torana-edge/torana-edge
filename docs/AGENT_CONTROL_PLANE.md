# Agent-facing Control Plane

Torana exposes the same local control plane used by its Web UI as a versioned,
JSON-first HTTP API. It is intended for shell scripts and software agents that
need to inspect or administer a personal Torana instance without scraping HTML.

For terminal and harness workflows, start with the [CLI guide](CLI.md).
`torana agent discover` reads this discovery document; dedicated commands cover
settings, plugin approval and ordering, configuration, statistics, and events.

This is an **operator API**, not the narrower MCP tool interface. Its loopback
and browser-origin checks prevent cross-site requests and DNS rebinding; they
do not authenticate or isolate programs running as your user. A harness with
local shell/network access can administer Torana here. Use its permission
prompts and sandbox restrictions to control that access. See
[security boundaries](../SECURITY.md).

Configuration and plugin-list GETs return an `ETag`. Every settings, pipeline,
or per-plugin configuration mutation **requires** that exact token as
`If-Match`, including on the unversioned routes. Missing or empty tokens return
HTTP 428 (`revision_required`); stale tokens return HTTP 412 (`stale_revision`).
Wildcards, weak tags, tag lists, and multiple `If-Match` headers are not snapshot
tokens and are rejected. Neither failure applies or persists changes.

The CLI and Web UI send revisions automatically. Other clients must retain the
ETag from the snapshot they actually inspected, not fetch a new token immediately
before submitting old edits. The token is opaque, process-specific, and covers
the whole managed configuration, so changes across endpoints and server restarts
invalidate older snapshots. Read a fresh snapshot, review it, and reapply the
intended edits after a conflict; do not retry automatically.

For a discovered plugin operation, `X-Torana-Plugin-Digest` optionally binds
the call to its loaded bundle digest. The CLI supplies it automatically.
A different loaded digest returns HTTP 412 and `stale_plugin_digest` before
the guest executes; rediscover and review instead of retrying blindly.

## Discover capabilities

The discovery document is the starting point:

```bash
TORANA_URL=http://127.0.0.1:8080
curl --fail-with-body --silent \
  "$TORANA_URL/_torana/api/v1/" | jq
```

It reports:

- the API version and canonical base path;
- the loopback-only security boundary;
- stable operation IDs;
- HTTP methods and paths;
- `read`, `write`, or `destructive` risk classification;
- idempotency;
- JSON input and output schemas;
- `revision_precondition` on snapshot mutations, naming the required header
  (`If-Match`) and `read_path` whose GET supplies the snapshot's ETag;
- operations contributed by enabled plugins, with the loaded bundle digest.

Only currently enabled plugin operations appear. A disabled plugin cannot
advertise a callable capability.

## Built-in examples

Read the redacted effective configuration:

```bash
curl --fail-with-body --silent \
  "$TORANA_URL/_torana/api/v1/config" | jq
```

List installed and enabled plugins, their exact bundle digests, requested
permissions, and operator approvals:

```bash
curl --fail-with-body --silent \
  "$TORANA_URL/_torana/api/v1/plugins" | jq
```

Read recent request events:

```bash
curl --fail-with-body --silent \
  "$TORANA_URL/_torana/api/v1/feed" | jq
```

Canonical v1 failures use a deterministic JSON envelope:

```json
{
  "error": {
    "code": "invalid_request",
    "message": "invalid json body"
  }
}
```

Callers must still check the HTTP status. Error codes are stable machine
categories; the message provides operator-readable detail.

Non-browser `POST`, `PUT`, `PATCH`, and `DELETE` calls must identify themselves
with `X-Torana-Local-Request: 1`. For a settings update, first save the snapshot
and its headers together:

```bash
curl --fail-with-body --silent --show-error \
  --dump-header config.headers --output config.json \
  "$TORANA_URL/_torana/api/v1/config"
```

Edit and review `config.json`, keeping `config.headers` from that read. Then
submit the edited snapshot with its original revision:

```bash
TORANA_REVISION=$(awk 'tolower($1) == "etag:" {sub(/\r$/, "", $2); print $2}' config.headers)
: "${TORANA_REVISION:?Missing ETag; read the snapshot again before editing}"
curl --fail-with-body --silent \
  -H 'X-Torana-Local-Request: 1' \
  -H 'Content-Type: application/json' \
  -H "If-Match: $TORANA_REVISION" \
  -X PUT \
  --data-binary @config.json \
  "$TORANA_URL/_torana/api/v1/config" | jq
```

The local-request header does not bypass localhost enforcement. It distinguishes deliberate
local automation from a browser request that omitted same-origin metadata.
Configuration revisions are separate from this caller check. Plugin-contributed
operations and identity-bound process shutdown do not require a configuration
revision; use each operation's advertised contract.

## Security boundary

The embedded control plane is localhost-only. Torana rejects:

- every non-loopback caller;
- DNS-rebinding requests with a foreign `Host`;
- cross-origin mutation requests.

The agent API does not make the control plane safe to expose publicly. If an
agent runs on another machine, use an operator-controlled local tunnel whose
remote end is not exposed to untrusted clients.

Mutating operations persist managed configuration and can rebuild live runtime
state. Agents should inspect each operation's `risk` and `idempotent` fields
before calling it. Digest-bound plugin approval remains an operator decision;
agents should not automatically approve unknown bundles.

## Plugin-contributed operations

A plugin can optionally ship `agent.json` beside `plugin.json`, `plugin.wasm`,
and `schema.json`:

```json
{
  "schema_version": 1,
  "description": "Machine-readable operations for this plugin.",
  "operations": [
    {
      "id": "status",
      "method": "GET",
      "path": "/status",
      "description": "Read plugin readiness.",
      "risk": "read",
      "idempotent": true,
      "output_schema": {
        "type": "object",
        "required": ["status"],
        "properties": {
          "status": {"type": "string"}
        },
        "additionalProperties": false
      }
    }
  ]
}
```

The plugin must also declare `run_on_http_request` and request
`env.serve_http`. An advertised `/status` operation is called at:

```text
GET /_torana/api/v1/agent/plugins/<plugin-name>/status
```

Torana dispatches that call to the plugin's existing isolated HTTP hook with
the guest request path rewritten to `/agent/status`. This keeps page routes
such as `/` separate from machine routes. Discovery operation IDs use the
unambiguous reserved `plugin:<plugin-name>:<operation-id>` namespace; built-in IDs use
`torana.*`, so community plugins cannot shadow host operations.

The guest must return a `ServeHTTP` result containing a valid JSON body. Torana
validates request and response values against the advertised schema and rejects
schema violations, oversized or non-JSON bodies, invalid status codes, and
unsafe response headers. Agent responses may only declare an
`application/json` content type; Torana rejects CORS, cache, framing, encoding,
cookie, length, and other plugin-supplied headers. Plugin operations must return
a JSON `2xx` success; redirects and plugin error statuses become Torana's
canonical `502 plugin_operation_failed` envelope.

Example Go SDK handler:

```go
sdk.OnHTTPRequest(func(ctx context.Context, req *pb.HttpRequest) (sdk.HTTPResult, error) {
	if req.Path != "/agent/status" {
		return sdk.PassHTTP(), nil
	}
	headers, _ := json.Marshal(map[string][]string{
		"Content-Type": {"application/json"},
	})
	return sdk.ServeHTTP(&pb.HttpResponse{
		Status:      200,
		HeadersJson: headers,
		Body:        []byte(`{"status":"ready"}`),
	}), nil
})
```

### Descriptor constraints

- `schema_version` must be `1`.
- Operation IDs contain at most 64 ASCII letters, digits, `.`, `_`, or `-`.
- Methods are `GET`, `POST`, `PUT`, `PATCH`, or `DELETE`.
- Paths are absolute plugin-relative paths without traversal, query, or
  fragment syntax.
- Each method/path pair and operation ID is unique.
- Input and output schemas use Torana's enforceable JSON Schema subset. Output
  schema is required. Without `input_schema`, the operation accepts no body.
- `GET` operations use `risk: "read"`.
- Mutations use `risk: "write"` or `"destructive"` as appropriate.

The v1 schema subset supports `type`, `properties`, `required`,
`additionalProperties` (boolean), `items`, `const`, `enum`, `$schema`, `title`,
and `description`. Types are `object`, `array`, `string`, `number`, `integer`,
`boolean`, and `null`. Unknown keywords are rejected at discovery so a plugin
cannot advertise constraints the host does not enforce.

`agent.json` is included in the plugin bundle digest. Any change to an
advertised operation, schema, risk, or description invalidates the prior
operator approval and requires review of the new exact bundle.
