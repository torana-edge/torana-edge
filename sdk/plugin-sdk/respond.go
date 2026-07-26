package plugin_sdk

import (
	"encoding/json"

	"github.com/torana-edge/torana-edge/sdk/pb"
)

// RespondRequest serves the response directly from the plugin: the proxy
// renders `content` as a complete, provider-shaped chat completion (SSE when
// the client requested streaming) and returns it to the caller. The upstream
// provider is never called and no tokens are spent — this is the primitive
// behind response caching, mock mode, and canned replies.
//
// Requires the env.respond_request permission grant; the proxy ignores the
// verdict unless a loaded plugin declares it. The plugin must return the same
// *req from its OnBeforeRequest handler for the verdict to take effect.
// If both a block and a respond verdict are set, the block wins.
func RespondRequest(req *pb.ChatRequest, content string) {
	meta := map[string]any{}
	if len(req.ToranaMetaJson) > 0 {
		_ = json.Unmarshal(req.ToranaMetaJson, &meta)
	}
	meta["_respond"] = map[string]any{"content": content}
	if b, err := json.Marshal(meta); err == nil {
		req.ToranaMetaJson = b
	}
}
