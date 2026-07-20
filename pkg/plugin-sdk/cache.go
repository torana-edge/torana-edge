package plugin_sdk

import (
	"encoding/json"

	"github.com/torana-edge/torana-edge/pkg/pb"
)

// Prompt-cache compliance
//
// Cache breakpoints (Message.CacheControlJson / ToolDef.CacheControlJson)
// survive the plugin boundary automatically — a plugin that returns a request
// preserves them without doing anything. What a plugin MUST guarantee is
// determinism: any mutation of the cacheable prefix (tools, system, history)
// must be a pure function of its input. Injecting wall-clock time, random
// values, or per-request-varying content before a breakpoint busts the
// provider's prompt cache every turn, silently multiplying token spend.
//
// A plugin that restructures messages (splits, merges, reorders) should use
// the helpers below to carry breakpoints to the equivalent position in its
// output rather than dropping them.

// CacheControl returns the message's cache breakpoint as a decoded map, or
// nil if the message carries none.
func CacheControl(msg *pb.Message) map[string]any {
	if msg == nil || len(msg.CacheControlJson) == 0 {
		return nil
	}
	var cc map[string]any
	if err := json.Unmarshal(msg.CacheControlJson, &cc); err != nil {
		return nil
	}
	return cc
}

// SetCacheBreakpoint attaches a cache breakpoint to the message. A nil or
// empty map clears it. Use the provider-default shape
// {"type":"ephemeral"} unless the original marker (from CacheControl) is
// being moved.
func SetCacheBreakpoint(msg *pb.Message, cc map[string]any) {
	if msg == nil {
		return
	}
	if len(cc) == 0 {
		msg.CacheControlJson = nil
		return
	}
	if b, err := json.Marshal(cc); err == nil {
		msg.CacheControlJson = b
	}
}

// MoveCacheBreakpoint transfers a breakpoint from one message to another
// (e.g. when a plugin merges the marked message into a neighbor). No-op if
// the source has no marker.
func MoveCacheBreakpoint(from, to *pb.Message) {
	if from == nil || to == nil || len(from.CacheControlJson) == 0 {
		return
	}
	to.CacheControlJson = from.CacheControlJson
	from.CacheControlJson = nil
}
