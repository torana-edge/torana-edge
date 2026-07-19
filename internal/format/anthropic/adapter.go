// Package anthropic implements format.RequestAdapter for the Anthropic Messages API.
package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
)

// anthropicRequest mirrors the Anthropic Messages request JSON shape for
// easy unmarshal/marshal.
type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     *int               `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	System        []contentBlock     `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicToolDef `json:"tools,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	StopReason    string             `json:"-"`
}

type anthropicMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

// UnmarshalJSON handles string content (Claude Code style) and array content.
func (am *anthropicMessage) UnmarshalJSON(data []byte) error {
	type alias anthropicMessage
	var raw struct {
		alias
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*am = anthropicMessage(raw.alias)

	// Try string first.
	var s string
	if json.Unmarshal(raw.Content, &s) == nil {
		am.Content = []contentBlock{{Type: "text", Text: s}}
		return nil
	}

	// Try array.
	var blocks []contentBlock
	if err := json.Unmarshal(raw.Content, &blocks); err != nil {
		return fmt.Errorf("content: expected string or array: %w", err)
	}
	am.Content = blocks
	return nil
}

type contentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   any            `json:"content,omitempty"` // string or array of blocks
	Thinking  string         `json:"thinking,omitempty"`
	Signature string         `json:"signature,omitempty"`
	Data      string         `json:"data,omitempty"`
	// Cache breakpoint marker, e.g. {"type":"ephemeral"}. Preserved verbatim
	// (opaque map) so TTL variants pass through. Dropping it disables the
	// provider's prompt cache for the whole prefix.
	CacheControl map[string]any `json:"cache_control,omitempty"`
	// Also handle tool_result content as array of blocks (Anthropic supports both)
}

type anthropicToolDef struct {
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	InputSchema  map[string]any `json:"input_schema"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

// UnmarshalJSON handles the polymorphic tool_result content (string or array).
func (cb *contentBlock) UnmarshalJSON(data []byte) error {
	type rawBlock contentBlock // avoid recursion
	var raw struct {
		rawBlock
		Content json.RawMessage `json:"content,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*cb = contentBlock(raw.rawBlock)
	if raw.Content == nil {
		return nil
	}
	// Try string first, then array
	var s string
	if json.Unmarshal(raw.Content, &s) == nil {
		cb.Content = s
		return nil
	}
	var blocks []any
	if json.Unmarshal(raw.Content, &blocks) == nil {
		cb.Content = blocks
		return nil
	}
	return fmt.Errorf("content: expected string or array, got %s", string(raw.Content))
}

// MarshalJSON handles re-serializing content blocks.
func (cb contentBlock) MarshalJSON() ([]byte, error) {
	type alias contentBlock
	a := alias(cb)
	b, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	// Anthropic requires `input` on every tool_use block, even when the tool
	// takes no arguments. The struct's `omitempty` drops an empty map, yielding
	// a block with no input that the API rejects ("missing field `input`").
	// This surfaces multi-turn: the client replays prior no-arg tool calls in
	// history (and the intent plugin can strip "i" down to an empty object).
	// Re-inject it as {} when absent. Only tool_use requires this; text and
	// tool_result blocks must keep omitting input.
	if cb.Type == "tool_use" && len(cb.Input) == 0 {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		m["input"] = json.RawMessage("{}")
		return json.Marshal(m)
	}
	return b, nil
}

// Adapter implements format.RequestAdapter for Anthropic Messages.
type Adapter struct{}

func init() {
	format.Register("/anthropic", format.Format{
		Name:    "anthropic",
		Request: &Adapter{},
		Stream:  &StreamAdapter{},
	})
}

// Unmarshal parses an Anthropic Messages request into a canonical ChatRequest.
func (a *Adapter) Unmarshal(rawBody []byte) (*engine.ChatRequest, error) {
	var ar anthropicRequest
	if err := json.Unmarshal(rawBody, &ar); err != nil {
		return nil, fmt.Errorf("anthropic unmarshal: %w", err)
	}

	chat := &engine.ChatRequest{
		Model:         ar.Model,
		Stream:        ar.Stream,
		MaxTokens:     ar.MaxTokens,
		Temperature:   ar.Temperature,
		TopP:          ar.TopP,
		StopSequences: ar.StopSequences,
	}

	var raw map[string]any
	if err := json.Unmarshal(rawBody, &raw); err == nil {
		delete(raw, "model")
		delete(raw, "max_tokens")
		delete(raw, "temperature")
		delete(raw, "top_p")
		delete(raw, "stop_sequences")
		delete(raw, "system")
		delete(raw, "messages")
		delete(raw, "tools")
		delete(raw, "stream")
		if len(raw) > 0 {
			chat.ProviderExtensions = raw
		}
	}

	// System: concatenate text blocks into first message. A cache breakpoint
	// on any system block (Claude Code marks the last one) is carried on the
	// coalesced message; Marshal re-emits it on the last system block, which
	// preserves the breakpoint position after coalescing.
	if len(ar.System) > 0 {
		var sysText []string
		var sysCache map[string]any
		for _, b := range ar.System {
			if b.Type == "text" && b.Text != "" {
				sysText = append(sysText, b.Text)
			}
			if b.CacheControl != nil {
				sysCache = b.CacheControl
			}
		}
		if len(sysText) > 0 {
			chat.Messages = append(chat.Messages, engine.Message{
				Role:         engine.RoleSystem,
				Content:      joinStrings(sysText, "\n"),
				CacheControl: sysCache,
			})
		}
	}

	// Messages: flatten content blocks.
	for _, am := range ar.Messages {
		role := mapRole(am.Role)
		var textParts []string
		var contentParts []any
		var toolCalls []engine.ToolCall
		var toolResults []engine.Message
		var thinking, thinkingSignature, redactedThinking string
		// resultsFirst records whether the original message opened with
		// tool_result blocks (Claude Code sends [tool_result..., text] — the
		// text is its injected context). The IR split must keep that order:
		// re-marshaling the text BEFORE the results interposes a user message
		// between the assistant's tool_use and its tool_results, which strict
		// providers reject ("tool_use ids were found without tool_result
		// blocks immediately after").
		resultsFirst := false
		sawContent := false
		// contentCache carries a cache breakpoint from a non-tool_result block
		// onto the coalesced content message (tool_result markers stay on their
		// own tool message, keeping the breakpoint's position exact).
		var contentCache map[string]any

		for _, block := range am.Content {
			switch block.Type {
			case "text":
				sawContent = true
				if block.Text != "" {
					textParts = append(textParts, block.Text)
				}
			case "image":
				sawContent = true
				contentParts = append(contentParts, block)
			case "tool_use":
				toolCalls = append(toolCalls, engine.ToolCall{
					ID:        block.ID,
					Name:      block.Name,
					Arguments: block.Input,
				})
			case "tool_result":
				if !sawContent {
					resultsFirst = true
				}
				tr := engine.Message{
					Role:         engine.RoleTool,
					ToolCallID:   block.ToolUseID,
					ToolName:     block.Name,
					CacheControl: block.CacheControl,
				}
				if s, ok := block.Content.(string); ok {
					tr.Content = s
				} else if arr, ok := block.Content.([]any); ok {
					tr.ContentParts = arr
				}
				toolResults = append(toolResults, tr)
			case "thinking":
				thinking = block.Thinking
				thinkingSignature = block.Signature
			case "redacted_thinking":
				redactedThinking = block.Data
			}
			// image blocks travel verbatim inside ContentParts (marker
			// included), tool_result markers ride their own tool message.
			if block.Type != "tool_result" && block.Type != "image" && block.CacheControl != nil {
				contentCache = block.CacheControl
			}
		}

		contentMsg := engine.Message{
			Role:              role,
			Content:           joinStrings(textParts, ""),
			ContentParts:      contentParts,
			ToolCalls:         toolCalls,
			Thinking:          thinking,
			ThinkingSignature: thinkingSignature,
			RedactedThinking:  redactedThinking,
			CacheControl:      contentCache,
		}
		hasContent := len(textParts) > 0 || len(contentParts) > 0 || len(toolCalls) > 0 || thinking != "" || redactedThinking != ""
		if resultsFirst {
			chat.Messages = append(chat.Messages, toolResults...)
			if hasContent {
				chat.Messages = append(chat.Messages, contentMsg)
			}
		} else {
			if hasContent {
				chat.Messages = append(chat.Messages, contentMsg)
			}
			chat.Messages = append(chat.Messages, toolResults...)
		}
	}

	// Tools.
	for _, t := range ar.Tools {
		chat.Tools = append(chat.Tools, engine.ToolDef{
			Name:         t.Name,
			Description:  t.Description,
			Parameters:   t.InputSchema,
			CacheControl: t.CacheControl,
		})
	}

	return chat, nil
}

// Marshal converts a canonical ChatRequest into Anthropic Messages JSON.
func (a *Adapter) Marshal(chat *engine.ChatRequest) ([]byte, error) {
	model := chat.Model
	if model == "" {
		model = "claude-sonnet-4-20250514"
	}
	ar := anthropicRequest{
		Model:         model,
		MaxTokens:     chat.MaxTokens,
		Temperature:   chat.Temperature,
		TopP:          chat.TopP,
		StopSequences: chat.StopSequences,
		Stream:        chat.Stream,
	}
	if ar.MaxTokens == nil {
		defaultMax := 4096
		ar.MaxTokens = &defaultMax
	}

	// System message: first Message with RoleSystem → system array.
	for _, m := range chat.Messages {
		if m.Role == engine.RoleSystem {
			ar.System = append(ar.System, contentBlock{
				Type:         "text",
				Text:         m.Content,
				CacheControl: m.CacheControl,
			})
		}
	}

	// Other messages → content blocks.
	//
	// Consecutive tool-result messages are coalesced into a SINGLE user
	// message. The Anthropic API requires every `tool_use` block in an
	// assistant turn to be answered by `tool_result` blocks in the one
	// immediately-following message. Emitting a separate user message per
	// tool result splits a parallel tool-call batch across several user
	// messages, so only the first result lands "in the next message" and the
	// rest are rejected:
	//   messages.N: `tool_use` ids were found without `tool_result` blocks
	//   immediately after ...
	// Coding agents (Claude Code) issue parallel tool calls constantly, so
	// this path is hit on essentially every multi-tool turn.
	for i := 0; i < len(chat.Messages); i++ {
		m := chat.Messages[i]
		if m.Role == engine.RoleSystem {
			continue // handled above
		}

		if m.Role == engine.RoleTool {
			am := anthropicMessage{Role: unmapRole(engine.RoleTool), Content: []contentBlock{}}
			for i < len(chat.Messages) && chat.Messages[i].Role == engine.RoleTool {
				tm := chat.Messages[i]
				if tm.Thinking != "" {
					am.Content = append(am.Content, thinkingBlock(tm))
				}
				cb := contentBlock{
					Type:         "tool_result",
					ToolUseID:    tm.ToolCallID,
					Name:         tm.ToolName,
					CacheControl: tm.CacheControl,
				}
				if len(tm.ContentParts) > 0 {
					cb.Content = tm.ContentParts
				} else {
					cb.Content = tm.Content
				}
				am.Content = append(am.Content, cb)
				i++
			}
			i-- // for-loop post-statement re-increments
			ar.Messages = append(ar.Messages, am)
			continue
		}

		am := anthropicMessage{
			Role: unmapRole(m.Role),
		}

		switch {
		case len(m.ToolCalls) > 0:
			// Assistant with tool calls.
			if m.Thinking != "" || m.RedactedThinking != "" {
				am.Content = append(am.Content, thinkingBlock(m))
			}
			if len(m.ContentParts) > 0 {
				for _, p := range m.ContentParts {
					b, _ := json.Marshal(p)
					var cb contentBlock
					json.Unmarshal(b, &cb)
					am.Content = append(am.Content, cb)
				}
			} else if m.Content != "" {
				am.Content = append(am.Content, contentBlock{
					Type: "text",
					Text: m.Content,
				})
			}
			for _, tc := range m.ToolCalls {
				am.Content = append(am.Content, contentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: tc.Arguments,
				})
			}
		default:
			// Simple text message.
			if m.Thinking != "" || m.RedactedThinking != "" {
				am.Content = append(am.Content, thinkingBlock(m))
			}
			if len(m.ContentParts) > 0 {
				for _, p := range m.ContentParts {
					b, _ := json.Marshal(p)
					var cb contentBlock
					json.Unmarshal(b, &cb)
					am.Content = append(am.Content, cb)
				}
			} else if m.Content != "" {
				am.Content = append(am.Content, contentBlock{
					Type: "text",
					Text: m.Content,
				})
			}
		}

		// Re-attach the message's cache breakpoint to the last emitted block —
		// breakpoints are positional ("cache everything up to here"), so after
		// block coalescing the end of the message is the faithful spot.
		if m.CacheControl != nil && len(am.Content) > 0 {
			last := &am.Content[len(am.Content)-1]
			if last.CacheControl == nil {
				last.CacheControl = m.CacheControl
			}
		}

		ar.Messages = append(ar.Messages, am)
	}

	// Tools.
	for _, t := range chat.Tools {
		ar.Tools = append(ar.Tools, anthropicToolDef{
			Name:         t.Name,
			Description:  t.Description,
			InputSchema:  t.Parameters,
			CacheControl: t.CacheControl,
		})
	}

	b, err := json.Marshal(ar)
	if err != nil {
		return nil, err
	}

	if len(chat.ProviderExtensions) > 0 {
		var outMap map[string]any
		json.Unmarshal(b, &outMap)
		for k, v := range chat.ProviderExtensions {
			outMap[k] = v
		}
		return json.Marshal(outMap)
	}

	return b, nil
}

// thinkingBlock returns the Anthropic content block for thinking/reasoning content.
func thinkingBlock(m engine.Message) contentBlock {
	if m.RedactedThinking != "" {
		return contentBlock{
			Type: "redacted_thinking",
			Data: m.RedactedThinking,
		}
	}
	return contentBlock{
		Type:      "thinking",
		Thinking:  m.Thinking,
		Signature: m.ThinkingSignature,
	}
}

func mapRole(r string) engine.Role {
	switch r {
	case "user":
		return engine.RoleUser
	case "assistant":
		return engine.RoleAssistant
	default:
		return engine.Role(r)
	}
}

func unmapRole(r engine.Role) string {
	switch r {
	case engine.RoleUser:
		return "user"
	case engine.RoleAssistant:
		return "assistant"
	case engine.RoleTool:
		return "user" // Anthropic tool_result messages use role:"user"
	default:
		return string(r)
	}
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for _, p := range parts[1:] {
		result += sep + p
	}
	return result
}
