package plugin

import (
	"context"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// TestOtelPluginEmitMetricABI loads the otel plugin and runs both hooks. This
// exercises the labeled emit_metric host ABI end-to-end: if the wasmimport
// signature and the host export disagree, the module traps at instantiation or
// on the call. With no OTel meter configured the host call is a safe no-op.
func TestOtelPluginEmitMetricABI(t *testing.T) {
	requireWASM(t, "../../plugins/otel/plugin.wasm")
	pp := newTestPipeline(t, "../../plugins", []string{"otel"})

	chat := &engine.ChatRequest{
		Model: "gpt-x",
		Messages: []engine.Message{
			{Role: engine.RoleUser, Content: "hello"},
		},
		Tools: []engine.ToolDef{{Name: "read"}},
	}
	if _, err := pp.RunBeforeRequest(context.Background(), 1, chat); err != nil {
		t.Fatalf("otel run_before_request: %v", err)
	}
	if _, err := pp.RunAfterResponse(context.Background(), 1, chat); err != nil {
		t.Fatalf("otel run_after_response: %v", err)
	}
}
