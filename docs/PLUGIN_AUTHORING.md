# Authoring External Plugins for Torana Edge

This guide provides an end-to-end Standard Operating Procedure (SOP) for authoring custom WASM plugins for Torana Edge in external repositories.

---

## Prerequisites

- **Go**: Version 1.23 or higher.
- **torana-cli**: (Optional) Installed or run via `go run ./cmd/torana-cli`.

---

## 1. Quickstart with `torana-cli`

You can scaffold a new external plugin directory using `torana-cli`:

```bash
torana-cli plugin init my-custom-plugin
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

2. Fetch the lightweight Torana Edge SDK:

```bash
go get github.com/torana-edge/torana-edge/sdk@latest
```

> **Note**: The `sdk` module contains only the plugin SDK and compiled Protobuf definitions, keeping external plugin dependencies minimal.

---

## 3. Writing Plugin Logic

Create `plugin.wasm.go`:

```go
package main

import (
	"context"
	"strings"

	"github.com/torana-edge/torana-edge/sdk/pb"
	sdk "github.com/torana-edge/torana-edge/sdk/plugin-sdk"
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
- `sdk.OnHttpRequest(fn func(ctx context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error))`

---

## 4. Writing the Manifest (`plugin.json`)

Every plugin directory must contain a `plugin.json` file describing its metadata, hooks, and required host permissions.

```json
{
  "name": "my-custom-plugin",
  "version": "0.1.0",
  "description": "Redacts sensitive terms from user prompts",
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
- **`version`**: Semantic version string (e.g. `"0.1.0"`).
- **`description`**: Human-readable description.
- **`hooks`**: Array of hook definitions:
  - **`name`**: Hook event type (`run_before_request`, `run_on_stream_chunk`, `run_on_http_request`).
  - **`priority`**: Execution order priority (`integer`). Lower numbers execute earlier.
- **`permissions`**: Declared host capabilities required by the plugin:
  - **`name`**: Capability permission string.
  - **`description`**: Rationale for requesting the capability.

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

Or using `torana-cli`:

```bash
torana-cli plugin build . -o plugin.wasm
```

---

## 6. Deployment and Activation

1. Place the compiled `plugin.wasm` and `plugin.json` into a subfolder under Torana's configured `plugins.dir`.
2. Update Torana's `config.json` or enable the plugin via the Torana Control Plane.
