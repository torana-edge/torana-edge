# Authoring External Plugins for Torana Edge

This guide provides an end-to-end Standard Operating Procedure (SOP) for authoring custom WASM plugins for Torana Edge in external repositories.

---

## Prerequisites

- **Go**: Version 1.23 or higher.
- **Torana CLI**: Installed as the same `torana` binary that runs the proxy.

---

## 1. Quickstart with `torana plugin`

You can scaffold a new external plugin directory using Torana:

```bash
torana plugin init my-custom-plugin
cd my-custom-plugin
```

This creates `go.mod`, `plugin.wasm.go`, and `plugin.json`.

---

## 2. Manual Project Setup

To create a new plugin project manually in an external repository:

1. Initialize a new Go module:

```bash
mkdir my-custom-plugin
cd my-custom-plugin
go mod init github.com/your-org/my-custom-plugin
```

2. Fetch the standalone Torana plugin SDK:

```bash
go get github.com/torana-edge/torana-plugin-sdk@latest
```

> **Note**: The SDK repository contains the ABI, helpers, templates, and
> conformance tests without pulling the proxy into your plugin module.

---

## 3. Writing Plugin Logic

Create `plugin.wasm.go`:

```go
package main

import (
	"context"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

func main() {}

func init() {
	// Register a hook to run before chat completion requests are forwarded upstream.
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		modified := false

		for _, msg := range req.Messages {
			if msg.Role == "user" && strings.Contains(msg.Content, "SECRET") {
				msg.Content = strings.ReplaceAll(msg.Content, "SECRET", "[REDACTED]")
				modified = true
			}
		}

		if !modified {
			return nil, nil // Return nil, nil if request was not modified
		}
		return req, nil
	})
}
```

### SDK Hook Signatures

- `sdk.OnBeforeRequest(fn func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error))`
- `sdk.OnStreamChunk(fn func(ctx context.Context, chunk *pb.StreamEvent) (*pb.StreamEventResult, error))`
- `sdk.OnHTTPRequest(fn func(ctx context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error))`

---

## 4. Writing the Manifest (`plugin.json`)

Every plugin directory must contain a `plugin.json` file describing its metadata, hooks, and required host permissions.

```json
{
  "schema_version": 1,
  "id": "my-custom-plugin",
  "name": "my-custom-plugin",
  "version": "0.1.0",
  "description": "Redacts sensitive terms from user prompts",
  "abi_version": "v1",
  "minimum_torana_version": "0.1.0",
  "failure_mode": "block",
  "repository": "https://github.com/your-org/my-custom-plugin",
  "hooks": [
    { "name": "run_before_request", "priority": 100 }
  ],
  "permissions": [
    { "name": "env.log", "description": "Emit diagnostic logs" }
  ]
}
```

### Manifest Schema Reference

- **`name`**: Unique string identifier for the plugin.
- **`schema_version`**: Manifest schema version. Use `1`.
- **`id`**: Stable machine-readable plugin identifier.
- **`version`**: Semantic version string (e.g. `"0.1.0"`).
- **`description`**: Human-readable description.
- **`abi_version`**: Torana plugin ABI version. Use `"v1"`.
- **`minimum_torana_version`**: Oldest compatible Torana release.
- **`failure_mode`**: Recommended operator policy, `"pass"` or `"block"`.
- **`repository`**: HTTPS source repository for provenance and support.
- **`hooks`**: Array of hook definitions:
  - **`name`**: Hook event type (`run_before_request`, `run_on_stream_chunk`, `run_on_http_request`).
  - **`priority`**: Execution order priority (`integer`). Lower numbers execute earlier.
- **`permissions`**: Declared host capabilities required by the plugin:
  - **`name`**: Capability permission string.
  - **`description`**: Rationale for requesting the capability.

Manifest permissions are requests, not grants. In production, Torana only
exposes capabilities present in an operator-owned approval, and that approval
is bound to the digest of the exact `plugin.json`, `plugin.wasm`,
`schema.json`, and optional `agent.json` bundle. A changed bundle must be
reviewed and approved again.
The Control Plane shows the digest and requested capabilities before enabling a
plugin.

Wazero's linear-memory isolation, execution timeout, and memory limit sandbox
untrusted guest code. Capability approvals separately limit which Torana host
operations the guest may invoke; they do not make an approved plugin trustworthy
or review its request/response transformation logic. Only install artifacts you
intend to run, grant the minimum requested subset, and prefer `failure_mode:
"block"` when silent pass-through would be unsafe.

### Available Capability Strings

| Capability | Description |
| --- | --- |
| `env.block_request` | Ability to block request processing with an error response. |
| `env.respond_request` | Ability to directly return a custom chat response without proxying. |
| `env.route_request` | Ability to override target upstream routing. |
| `env.serve_http` | Ability to handle standalone HTTP endpoints on Torana. |
| `env.emit_metric` | Ability to emit OTel metrics via `sdk.EmitMetric`. |
| `env.log` | Ability to write diagnostic logs via `sdk.Log`. |
| `env.host_call.*` | Custom host calls (e.g. `env.cache_get`, `env.cache_set`, `env.meta_get`, `env.meta_set`, `env.host_call.torana_record_savings`). |

---

## 5. Building the Plugin WASM

Build the WebAssembly binary targeting WASI (`wasip1`):

```bash
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
```

Or using Torana:

```bash
torana plugin build . -o plugin.wasm
```

---

## 6. Deployment and Activation

1. Place `plugin.wasm`, `plugin.json`, `schema.json`, and optional `agent.json`
   into a subfolder under Torana's configured `plugins.dir`.
2. Open the Torana Control Plane, inspect the bundle digest and requested
   capabilities, choose a failure policy, and approve the plugin.
3. Enable and order the approved plugin. Torana persists the operator approval
   separately from the plugin-owned manifest.

## 7. Optional agent-facing operations

Plugins that already vend a page through `run_on_http_request` can also expose
machine-readable operations. Add a language-neutral `agent.json` descriptor and
handle the advertised path beneath `/agent` in the same HTTP hook. Torana
aggregates enabled operations in `GET /_torana/api/v1/`, enforces JSON
responses, and includes the descriptor in the digest-bound approval.

See [AGENT_CONTROL_PLANE.md](AGENT_CONTROL_PLANE.md) for the descriptor schema,
dispatch contract, validation rules, and a complete curl example.
