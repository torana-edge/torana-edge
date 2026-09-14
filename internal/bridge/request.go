package bridge

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format"
)

// RequestOptions are explicit operator defaults, never inferred model limits.
type RequestOptions struct {
	MaxTokens int
	Project   string
}

// ParseRequest uses the explicitly selected API, rather than guessing a wire
// variant from an ambiguous body. The path owns Gemini's model and stream mode.
func ParseRequest(p Protocol, body []byte, path string) (*engine.ChatRequest, error) {
	if !p.Valid() {
		return nil, unsupported("unknown client protocol")
	}
	if err := validateRequestShape(p, body); err != nil {
		return nil, err
	}
	f := format.Lookup(p.Format())
	if f == nil {
		return nil, unsupported("unregistered client protocol")
	}
	if p == Gemini || p == GeminiCodeAssist {
		var err error
		body, err = normalizeGeminiRequestSchemas(body, p)
		if err != nil {
			return nil, err
		}
	}
	chat, err := f.Request.Unmarshal(body)
	if err != nil {
		return nil, unsupported("malformed client request")
	}
	if (p == OpenAIResponses) != (chat.OpenAIVariant == engine.OpenAIResponses) && p.Format() == "openai" {
		return nil, unsupported("request body for a different client protocol")
	}
	if p == Gemini || p == GeminiCodeAssist {
		if chat.CodeAssist != (p == GeminiCodeAssist) {
			return nil, unsupported("request envelope for a different client protocol")
		}
		chat.Stream = strings.HasSuffix(strings.TrimSuffix(path, "/"), ":streamGenerateContent")
		if p == Gemini {
			prefix, _, ok := strings.Cut(path, ":")
			if !ok {
				return nil, unsupported("Gemini inference path")
			}
			_, name, ok := strings.Cut(prefix, "/models/")
			if !ok || name == "" || strings.Contains(name, "/") {
				return nil, unsupported("Gemini model path")
			}
			chat.Model = name
		}
	}
	return chat, nil
}

// ProjectRequest creates an owned, destination-shaped IR. It never mutates the
// accepted client request or copies provider-specific extensions across APIs.
func ProjectRequest(chat *engine.ChatRequest, from, to Protocol, opts RequestOptions) (*engine.ChatRequest, error) {
	if chat == nil || !from.Valid() || !to.Valid() {
		return nil, unsupported("unknown request protocol")
	}
	wire, err := pbconv.ToPBChatRequestChecked(chat)
	if err != nil {
		return nil, unsupported("invalid canonical request")
	}
	out, err := pbconv.FromPBChatRequest(wire)
	if err != nil {
		return nil, unsupported("invalid canonical request")
	}
	out.ResponsesInputLayout = chat.ResponsesInputLayout
	out.ResponsesInstructions = chat.ResponsesInstructions
	to.ApplyTopology(out)
	if from == to {
		return out, nil
	}
	if !out.SafetySettings.IsAbsent() && !geminiPair(from, to) {
		return nil, unsupported("provider safety settings")
	}
	if err := projectExtensions(out, from, to); err != nil {
		return nil, err
	}
	if err := projectMessages(out, from, to); err != nil {
		return nil, err
	}
	for i := range out.Tools {
		tool := &out.Tools[i]
		if tool.Name == "" {
			return nil, unsupported("unnamed tools")
		}
		if err := validateToolIdentity(to, tool.Name, ""); err != nil {
			return nil, err
		}
		if tool.InvocationKind != engine.ToolInvocationFunction || len(tool.NamespacePath) != 0 {
			return nil, unsupported("free-form or namespaced tools")
		}
		if tool.Strict && to != OpenAIChat && to != OpenAIResponses {
			return nil, unsupported("strict function schemas on the upstream protocol")
		}
		if !tool.CacheControl.IsAbsent() && to != Anthropic {
			return nil, unsupported("provider cache breakpoints")
		}
	}
	out.ResponsesInputLayout = engine.OptionalJSONArray{}
	out.ResponsesInstructions = false
	if to == Anthropic && out.MaxTokens == nil {
		if opts.MaxTokens <= 0 || opts.MaxTokens > math.MaxInt32 {
			return nil, unsupported("Anthropic max_tokens; set bridge.max_tokens or send a token limit")
		}
		limit := opts.MaxTokens
		out.MaxTokens = &limit
	}
	if to == OpenAIResponses && len(out.StopSequences) != 0 {
		return nil, unsupported("stop sequences on the Responses API")
	}
	if to == GeminiCodeAssist {
		if strings.TrimSpace(opts.Project) == "" {
			return nil, unsupported("Code Assist project; set bridge.project")
		}
		members, _, _ := out.ProviderExtensions.DecodeObject()
		if _, ok := members["request"]; !ok {
			out.ProviderExtensions, err = out.ProviderExtensions.SetMember("request", []byte(`{}`))
			if err != nil {
				return nil, err
			}
		}
		raw, _ := json.Marshal(opts.Project)
		out.ProviderExtensions, err = out.ProviderExtensions.SetMember("project", raw)
		if err != nil {
			return nil, err
		}
	}
	if err := validateProjectedToolDefinitionsAndChoice(out, to); err != nil {
		return nil, err
	}
	// Marshal now as a capability check; the proxy repeats it after its own
	// deterministic metering/compaction additions.
	f := format.Lookup(to.Format())
	if f == nil {
		return nil, unsupported("unregistered upstream protocol")
	}
	if _, err := MarshalRequest(to, out); err != nil {
		return nil, unsupported("request features on the upstream protocol")
	}
	return out, nil
}

func geminiPair(a, b Protocol) bool {
	return (a == Gemini || a == GeminiCodeAssist) && (b == Gemini || b == GeminiCodeAssist)
}

// Endpoint returns a decoded URL.Path relative to the configured provider base
// path; net/url escapes it when writing the request. A base ending in
// /v1 should use an origin instead: protocol endpoints already include a version.
func Endpoint(p Protocol, model string, stream bool) (string, string, error) {
	switch p {
	case OpenAIChat:
		return "/v1/chat/completions", "", nil
	case OpenAIResponses:
		return "/v1/responses", "", nil
	case Anthropic:
		return "/v1/messages", "", nil
	case Gemini:
		if model == "" || strings.ContainsAny(model, "/?#%:") || model == "." || model == ".." {
			return "", "", unsupported("upstream Gemini model identifier")
		}
		suffix, query := ":generateContent", ""
		if stream {
			suffix, query = ":streamGenerateContent", "alt=sse"
		}
		return "/v1beta/models/" + model + suffix, query, nil
	case GeminiCodeAssist:
		if stream {
			return "/v1internal:streamGenerateContent", "alt=sse", nil
		}
		return "/v1internal:generateContent", "", nil
	default:
		return "", "", unsupported("unknown upstream protocol")
	}
}

func projectMessages(chat *engine.ChatRequest, from, to Protocol) error {
	calls := map[string]string{}
	seenCalls := map[string]bool{}
	resultIDs := map[string]bool{}
	messages := make([]engine.Message, 0, len(chat.Messages))
	var system []engine.Block
	seenConversation := false
	for _, msg := range chat.Messages {
		switch msg.Role {
		case engine.RoleSystem, engine.RoleUser, engine.RoleAssistant, engine.RoleTool:
		case "developer":
			if to != OpenAIChat && to != OpenAIResponses {
				return unsupported("developer-role instructions on the upstream protocol")
			}
		default:
			return unsupported("message role")
		}
		// A system message occurring later in a conversation must not move ahead
		// of earlier user turns when a protocol has only a preamble.
		if msg.Role == engine.RoleSystem {
			if seenConversation && (to == Anthropic || to == Gemini || to == GeminiCodeAssist) {
				return unsupported("mid-conversation system instructions")
			}
		} else {
			seenConversation = true
		}
		for i := range msg.Blocks {
			b := &msg.Blocks[i]
			switch {
			case b.Text != nil:
				if b.Text.Signature != "" || !b.Text.PartMetadataJson.IsAbsent() {
					return unsupported("signed text or provider part metadata")
				}
			case b.ToolUse != nil:
				t := b.ToolUse
				if msg.Role != engine.RoleAssistant || t.Name == "" || t.ID == "" || seenCalls[t.ID] {
					return unsupported("ambiguous tool-call identity")
				}
				if t.Signature != "" || !t.PartMetadataJson.IsAbsent() {
					return unsupported("signed tool calls or provider part metadata")
				}
				if t.InvocationKind != engine.ToolInvocationFunction {
					return unsupported("free-form tool calls")
				}
				if err := validateToolIdentity(to, t.Name, t.ID); err != nil {
					return err
				}
				calls[t.ID], seenCalls[t.ID] = t.Name, true
			case b.ToolResult != nil:
				r := b.ToolResult
				if r.Signature != "" || !r.PartMetadataJson.IsAbsent() || r.WillContinue != nil || r.Scheduling != nil {
					return unsupported("provider-specific tool-result state")
				}
				if r.InvocationKind != engine.ToolInvocationFunction {
					return unsupported("free-form tool results")
				}
				if r.IsError != nil && *r.IsError && to != Anthropic {
					return unsupported("tool-result error flags on the upstream protocol")
				}
				name, exists := calls[r.ToolCallID]
				if !exists || resultIDs[r.ToolCallID] || (r.ToolName != "" && r.ToolName != name) {
					return unsupported("unmatched or ambiguous tool results")
				}
				resultIDs[r.ToolCallID] = true
				if to == Gemini || to == GeminiCodeAssist {
					r.ToolName = name
				}
				if to == Anthropic {
					r.ToolName = ""
				}
				for j := range r.Content {
					c := &r.Content[j]
					if c.CacheBreakpoint != nil {
						return unsupported("provider cache breakpoints")
					}
					if c.Unknown != nil {
						return unsupported("multimodal tool results")
					}
				}
				// Gemini accepts one object result, so combine the ordered text payload
				// once; its native adapter wraps non-object text under output/content.
				if to == Gemini || to == GeminiCodeAssist {
					var text strings.Builder
					for _, c := range r.Content {
						text.WriteString(c.Text)
					}
					r.Content = []engine.ToolResultContentBlock{{Text: text.String()}}
				}
			case b.Unknown != nil:
				converted, err := projectMedia(b.Unknown, from, to)
				if err != nil {
					return err
				}
				b.Unknown = converted
			case b.CacheBreakpoint != nil:
				return unsupported("provider cache breakpoints")
			default:
				return unsupported("reasoning, signatures, or opaque conversation state")
			}
		}
		if msg.Role == engine.RoleSystem && (to == Anthropic || to == Gemini || to == GeminiCodeAssist) {
			system = append(system, msg.Blocks...)
			continue
		}
		if to == OpenAIChat || to == OpenAIResponses {
			// Anthropic places results inside a user turn. OpenAI gives each result
			// its own tool message; split in wire order without moving user text.
			var pending []engine.Block
			flush := func() {
				if len(pending) > 0 {
					messages = append(messages, engine.Message{Role: msg.Role, Blocks: pending})
					pending = nil
				}
			}
			for _, block := range msg.Blocks {
				if block.ToolResult != nil {
					flush()
					messages = append(messages, engine.Message{Role: engine.RoleTool, Blocks: []engine.Block{block}})
				} else {
					pending = append(pending, block)
				}
			}
			flush()
		} else {
			// A Code Assist result arrives in a model turn. Other contracts
			// put it on the user side; split at the block boundary before the
			// destination adapter rebuilds its native role topology.
			for _, block := range msg.Blocks {
				role := msg.Role
				if block.ToolResult != nil || role == engine.RoleTool {
					role = engine.RoleUser
				}
				if len(messages) > 0 && messages[len(messages)-1].Role == role {
					messages[len(messages)-1].Blocks = append(messages[len(messages)-1].Blocks, block)
				} else {
					messages = append(messages, engine.Message{Role: role, Blocks: []engine.Block{block}})
				}
			}
		}
	}
	if len(system) > 0 {
		messages = append([]engine.Message{{Role: engine.RoleSystem, Blocks: system}}, messages...)
	}
	chat.Messages = messages
	return nil
}
