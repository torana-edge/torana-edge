package conversation

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
)

const maxExternalIdentityBytes = 1024

// Identity is the opaque Torana label plus the source that selected it. Source
// is diagnostic only and must never be treated as caller authority.
type Identity struct {
	ID     string
	Source string
}

type headerIdentity struct {
	name   string
	source string
}

// Keep this explicit and reviewable. New harness conventions can be added
// without changing the plugin ABI or the content-derived fallback.
var stableIdentityHeaders = []headerIdentity{
	{name: "X-Claude-Code-Session-Id", source: "claude-code-session"},
	{name: "X-Codex-Thread-Id", source: "codex-thread"},
	{name: "X-Codex-Session-Id", source: "codex-session"},
	{name: "Thread-Id", source: "harness-thread"},
	{name: "Session-Id", source: "harness-session"},
}

// ResolveIdentity prefers a stable ID explicitly supplied by the harness or
// provider. If none is available, it retains Torana's format-independent
// content-root derivation. Changing turns therefore do not move the fallback,
// while compaction-capable harnesses can keep identity stable even when they
// replace those roots.
func ResolveIdentity(headers http.Header, chat *engine.ChatRequest) Identity {
	for _, candidate := range stableIdentityHeaders {
		if value := validExternalValue(headers.Get(candidate.name)); value != "" {
			return external(candidate.source, value)
		}
	}
	if source, value := providerIdentity(chat); value != "" {
		return external(source, value)
	}
	if id := engine.ConversationID(chat); id != "" {
		return Identity{ID: id, Source: "content-root"}
	}
	return Identity{}
}

func external(source, value string) Identity {
	return Identity{ID: engine.ExternalConversationID(source, value), Source: source}
}

func validExternalValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxExternalIdentityBytes {
		return ""
	}
	return value
}

func providerIdentity(chat *engine.ChatRequest) (string, string) {
	if chat == nil || chat.ProviderExtensions.IsAbsent() {
		return "", ""
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(chat.ProviderExtensions.Bytes(), &root); err != nil {
		return "", ""
	}
	if chat.CodeAssist {
		if value := jsonStringMember(root, "sessionId"); value != "" {
			return "gemini-code-assist-session", value
		}
		var request map[string]json.RawMessage
		if raw := root["request"]; len(raw) > 0 && json.Unmarshal(raw, &request) == nil {
			if value := jsonStringMember(request, "sessionId"); value != "" {
				return "gemini-code-assist-session", value
			}
		}
	}
	if chat.OpenAIVariant == engine.OpenAIResponses {
		if value := responseConversationID(root["conversation"]); value != "" {
			return "openai-responses-conversation", value
		}
		if value := codexClientThreadID(root["client_metadata"]); value != "" {
			return "codex-client-thread", value
		}
		// A Responses delta continuation may contain only function-call output;
		// its earlier sanitized context lives at the provider and therefore is
		// not available for Torana's content-root fallback. The parent response
		// identifies this continuation/retry scope. Do not use it for full-history
		// requests, where it changes every turn and a stable content root exists.
		if deltaOnlyToolResults(chat) {
			if value := jsonStringMember(root, "previous_response_id"); value != "" {
				return "openai-responses-parent", value
			}
		}
	}
	return "", ""
}

func codexClientThreadID(raw json.RawMessage) string {
	var metadata map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &metadata) != nil {
		return ""
	}
	if value := jsonStringMember(metadata, "thread_id"); value != "" {
		return value
	}
	var nestedRaw string
	if json.Unmarshal(metadata["x-codex-turn-metadata"], &nestedRaw) != nil {
		return ""
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal([]byte(nestedRaw), &nested) != nil {
		return ""
	}
	return jsonStringMember(nested, "thread_id")
}

func deltaOnlyToolResults(chat *engine.ChatRequest) bool {
	if chat == nil || len(chat.Messages) == 0 {
		return false
	}
	found := false
	for _, message := range chat.Messages {
		if len(message.Blocks) == 0 {
			return false
		}
		for _, block := range message.Blocks {
			if block.ToolResult == nil {
				return false
			}
			found = true
		}
	}
	return found
}

func jsonStringMember(object map[string]json.RawMessage, name string) string {
	var value string
	if raw := object[name]; len(raw) > 0 && json.Unmarshal(raw, &value) == nil {
		return validExternalValue(value)
	}
	return ""
}

func responseConversationID(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return validExternalValue(value)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		return jsonStringMember(object, "id")
	}
	return ""
}
