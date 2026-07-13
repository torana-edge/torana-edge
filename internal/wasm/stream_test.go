package wasm

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestStreamMutation(t *testing.T) {
	b, err := os.ReadFile("../../examples/plugins/test-stream-mutator/plugin.wasm")
	if err != nil {
		t.Skipf("test-stream-mutator plugin.wasm not found: %v", err)
	}

	ctx := context.Background()
	r := NewRuntime(ctx)
	defer r.Close()

	p, err := r.LoadPlugin("test_stream_mutator", b)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		input    map[string]any
		expected map[string]any
	}{
		{
			name: "Mutate secret text delta",
			input: map[string]any{
				"TextDelta": "secret",
			},
			expected: map[string]any{
				"TextDelta": "[REDACTED]",
			},
		},
		{
			name: "Pass through normal text delta",
			input: map[string]any{
				"TextDelta": "hello world",
			},
			expected: map[string]any{
				"TextDelta": "hello world",
			},
		},
		{
			name: "Pass through ToolCallStart",
			input: map[string]any{
				"ToolCallStart": map[string]any{"Index": 0, "ID": "call_123", "Name": "search"},
			},
			expected: map[string]any{
				"ToolCallStart": map[string]any{"Index": float64(0), "ID": "call_123", "Name": "search"},
			},
		},
		{
			name: "Pass through ToolCallDelta",
			input: map[string]any{
				"ToolCallDelta": map[string]any{"Index": 0, "ArgumentsDelta": `{"qu`},
			},
			expected: map[string]any{
				"ToolCallDelta": map[string]any{"Index": float64(0), "ArgumentsDelta": `{"qu`},
			},
		},
		{
			name: "Pass through ToolCallEnd",
			input: map[string]any{
				"ToolCallEnd": map[string]any{"Index": 0},
			},
			expected: map[string]any{
				"ToolCallEnd": map[string]any{"Index": float64(0)},
			},
		},
		{
			name: "Pass through FinishReason",
			input: map[string]any{
				"FinishReason": "stop",
			},
			expected: map[string]any{
				"FinishReason": "stop",
			},
		},
		{
			name: "Pass through Usage",
			input: map[string]any{
				"Usage": map[string]any{"InputTokens": 10, "OutputTokens": 20},
			},
			expected: map[string]any{
				"Usage": map[string]any{"InputTokens": float64(10), "OutputTokens": float64(20)},
			},
		},
		{
			name: "Pass through Error",
			input: map[string]any{
				"Error": map[string]any{"Code": 500, "Message": "test error"},
			},
			expected: map[string]any{
				"Error": map[string]any{"Code": float64(500), "Message": "test error"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output map[string]any
			err := p.CallRequest(ctx, "on_stream_event", tt.input, &output)
			if err != nil {
				t.Fatalf("CallRequest failed: %v", err)
			}
			
			// If CallRequest didn't modify, output is empty (or nil).
			// We should simulate the passthrough behavior if nil.
			actual := tt.input
			if output != nil {
				actual = output
			}

			// Compare JSON representations to avoid type mismatch issues
			actualJSON, _ := json.Marshal(actual)
			expectedJSON, _ := json.Marshal(tt.expected)
			
			if string(actualJSON) != string(expectedJSON) {
				t.Errorf("Expected %s, got %s", expectedJSON, actualJSON)
			}
		})
	}
}
