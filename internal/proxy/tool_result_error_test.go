package proxy

import (
	"net/http"
	"sync/atomic"
	"testing"
)

func TestAnthropicToolFailureIsValidIngress(t *testing.T) {
	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","is_error":true,"content":"permission denied"}]}]}`
	status, response, hits, _ := parseFailE2E(t, "anthropic", nil, body)
	if status != http.StatusOK || atomic.LoadInt32(hits) != 1 {
		t.Fatalf("valid Anthropic error result rejected: status=%d upstream_hits=%d body=%s", status, atomic.LoadInt32(hits), response)
	}
}
