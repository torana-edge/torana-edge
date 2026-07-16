package plugin_sdk

import (
	"encoding/json"

	"github.com/torana-edge/torana-edge/pkg/pb"
)

// BlockRequest marks a request to be rejected before it reaches the upstream
// provider. The proxy short-circuits the request and returns a provider-shaped
// error carrying the given HTTP status, error code, and message.
//
// The message is surfaced to the caller (and, via the agent harness, to the
// upstream model on its next turn), so it MUST NOT contain sensitive values —
// describe what was found and where, never the value itself.
//
// Requires the env.block_request permission grant; the proxy ignores the block
// unless a loaded plugin declares it. The plugin must return the same *req from
// its OnBeforeRequest handler for the verdict to take effect.
func BlockRequest(req *pb.ChatRequest, status int32, code, message string) {
	meta := map[string]any{}
	if len(req.ToranaMetaJson) > 0 {
		_ = json.Unmarshal(req.ToranaMetaJson, &meta)
	}
	meta["_block"] = map[string]any{
		"status":  status,
		"code":    code,
		"message": message,
	}
	if b, err := json.Marshal(meta); err == nil {
		req.ToranaMetaJson = b
	}
}
