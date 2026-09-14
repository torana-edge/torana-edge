package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// TestCompiledRustRespondRequestThroughProductionRuntime exercises the Rust
// public helper through Edge's actual plugin loader, dispatcher, host-call
// validator, and attributed verdict store. SDK conformance also tests its
// guest directly; this test keeps the production integration boundary honest.
func TestCompiledRustRespondRequestThroughProductionRuntime(t *testing.T) {
	guest := os.Getenv("TORANA_RUST_GUEST")
	if guest == "" {
		if os.Getenv("TORANA_RUST_CONFORMANCE") == "1" {
			t.Fatal("TORANA_RUST_GUEST is required when TORANA_RUST_CONFORMANCE=1")
		}
		t.Skip("TORANA_RUST_GUEST unset; set it to the compiled SDK rust-allhooks guest")
	}
	wasmBytes, err := os.ReadFile(guest)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bundle := filepath.Join(dir, "rust-allhooks")
	if err := os.Mkdir(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schema_version":1,"id":"conformance/rust-allhooks","name":"rust-allhooks","version":"0.1.0","abi_version":"v1","failure_mode":"block","description":"Rust ABI-v1 conformance guest","hooks":[{"name":"run_before_request"},{"name":"run_after_response"},{"name":"run_on_stream_chunk"},{"name":"run_on_http_request"},{"name":"run_on_tick"}],"permissions":[{"name":"env.respond_request","description":"Exercise typed synthetic responses"}]}`
	schema := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"properties":{}}`
	for name, data := range map[string][]byte{"plugin.wasm": wasmBytes, "plugin.json": []byte(manifest), "schema.json": []byte(schema)} {
		if err := os.WriteFile(filepath.Join(bundle, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runtime := wasm.NewRuntime(context.Background())
	t.Cleanup(func() { _ = runtime.Close() })
	pp, err := NewPipeline(runtime, PluginConfig{Dir: dir, Order: []string{"rust-allhooks"}, AllowUnapproved: true})
	if err != nil {
		t.Fatal(err)
	}
	const reqID = 73
	request := &engine.ChatRequest{Model: "__respond_request_conformance__"}
	if _, err := pp.RunBeforeRequest(context.Background(), reqID, request, nil); err != nil {
		t.Fatal(err)
	}
	verdict := pp.Verdicts(reqID).Respond()
	if verdict == nil || verdict.Plugin != "rust-allhooks" || verdict.Response == nil || verdict.Response.Message == nil {
		t.Fatalf("respond verdict = %#v", verdict)
	}
	if verdict.Response.FinishReason != "stop" || len(verdict.Response.Message.Blocks) != 1 || verdict.Response.Message.Blocks[0].GetText().GetText() != "compiled Rust response" {
		t.Fatalf("synthetic response = %#v", verdict.Response)
	}
}
