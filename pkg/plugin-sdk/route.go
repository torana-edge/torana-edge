package plugin_sdk

import (
	"encoding/json"

	"github.com/torana-edge/torana-edge/pkg/pb"
)

// RouteRequest redirects this request to another configured provider and/or
// overrides the model — the content-based routing primitive: send trivial
// prompts to a cheap or local model, hard ones to a premium one.
//
// Rules enforced by the proxy:
//   - The target provider must exist in the config and use the SAME wire
//     format as the matched provider (cross-format transcoding is not
//     supported yet). Violations are logged and the original route is kept.
//   - The caller's credential is NEVER forwarded to a rerouted provider;
//     auth comes from the target's api_key_env (or nothing, for local
//     endpoints).
//   - An empty provider with a non-empty model is a model-only override on
//     the original provider.
//
// Requires the env.route_request permission grant; ignored otherwise. The
// plugin must return the same *req from its OnBeforeRequest handler.
func RouteRequest(req *pb.ChatRequest, provider, model string) {
	meta := map[string]any{}
	if len(req.ToranaMetaJson) > 0 {
		_ = json.Unmarshal(req.ToranaMetaJson, &meta)
	}
	meta["_route"] = map[string]any{"provider": provider, "model": model}
	if b, err := json.Marshal(meta); err == nil {
		req.ToranaMetaJson = b
	}
}
