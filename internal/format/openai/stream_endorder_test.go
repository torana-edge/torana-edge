package openai

import (
	"strings"
	"testing"
)

// TestToolCallEndOrderDeterministic: ToolCallEnd events for parallel tool
// calls are emitted in ascending index order (was map-iteration order).
func TestToolCallEndOrderDeterministic(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"a"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c1","function":{"name":"b"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"c2","function":{"name":"c"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":3,"id":"c3","function":{"name":"d"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n")

	for run := 0; run < 20; run++ {
		var ends []int
		for ev := range (&StreamAdapter{}).ParseStream(strings.NewReader(sse)) {
			if ev.ToolCallEnd != nil {
				ends = append(ends, ev.ToolCallEnd.Index)
			}
		}
		if len(ends) != 4 {
			t.Fatalf("run %d: expected 4 ends, got %v", run, ends)
		}
		for i, idx := range ends {
			if idx != i {
				t.Fatalf("run %d: ends out of order: %v", run, ends)
			}
		}
	}
}
