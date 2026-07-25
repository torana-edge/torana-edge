# Agent-facing Control Plane

Torana exposes the same local control plane used by its Web UI as a versioned,
JSON-first HTTP API. It is intended for shell scripts and software agents that
need to inspect or administer a personal Torana instance without scraping HTML.

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
explicitly:

```bash
curl --fail-with-body --silent \
  -H 'X-Torana-Local-Request: 1' \
  -H 'Content-Type: application/json' \
  -X PUT \
  --data-binary @config.json \
  "$TORANA_URL/_torana/api/v1/config" | jq
```

This header does not bypass localhost enforcement. It distinguishes deliberate
local automation from a browser request that omitted same-origin metadata.

## Security boundary

The embedded control plane is localhost-only. Torana rejects:

- non-loopback callers, including callers presenting legacy remote tokens;
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

The guest must return a handled `HttpResponse` with a valid JSON body. Torana
validates request and response values against the advertised schema and rejects
schema violations, oversized or non-JSON bodies, invalid status codes, and
unsafe response headers. Agent responses may only declare an
`application/json` content type; Torana rejects CORS, cache, framing, encoding,
cookie, length, and other plugin-supplied headers. Plugin operations must return
a JSON `2xx` success; redirects and plugin error statuses become Torana's
canonical `502 plugin_operation_failed` envelope.

Example Go SDK handler:

```go
sdk.OnHTTPRequest(func(ctx context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error) {
	if req.Path != "/agent/status" {
		return nil, nil
	}
	headers, _ := json.Marshal(map[string][]string{
		"Content-Type": {"application/json"},
	})
	return &pb.HttpResponse{
		Status:      200,
		HeadersJson: headers,
		Body:        []byte(`{"status":"ready"}`),
		Handled:     true,
	}, nil
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
