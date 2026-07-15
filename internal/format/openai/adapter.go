// Package openai implements format adapters for OpenAI-compatible APIs.
// It handles both the Chat Completions API and the Responses API, detecting
// which variant is in use from the JSON body structure.
package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
)

func init() {
	format.Register("/openai", format.Format{
		Name:    "openai",
		Request: &Adapter{},
		Stream:  &StreamAdapter{},
	})
}

// Adapter implements format.RequestAdapter for OpenAI Chat Completions
// and Responses API formats.
type Adapter struct{}

// --- wire types for unmarshal ------------------------------------------------

// chatRequest is the Chat Completions JSON shape.
type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Tools         []chatToolDef `json:"tools,omitempty"`
	Stream        bool          `json:"stream"`
	MaxTokens     *int          `json:"max_tokens,omitempty"`
	Temperature   *float64      `json:"temperature,omitempty"`
	TopP          *float64      `json:"top_p,omitempty"`
	StopSequences interface{}   `json:"stop,omitempty"`
}

type chatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatToolFunc `json:"function"`
}

type chatToolFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatToolDef struct {
	Type     string          `json:"type"`
	Function chatToolFuncDef `json:"function"`
	Strict   bool            `json:"strict,omitempty"`
}

type chatToolFuncDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// responseRequest is the Responses API JSON shape.
type responseRequest struct {
	Object string          `json:"object,omitempty"`
	Model  string          `json:"model,omitempty"`
	Input  json.RawMessage `json:"input"`
	Tools  []responseTool  `json:"tools,omitempty"`
	Stream bool            `json:"stream"`
}

type responseTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ---------------------------------------------------------------------------
// Unmarshal
// ---------------------------------------------------------------------------

// Unmarshal detects the API variant and parses rawBody into a ChatRequest.
func (a *Adapter) Unmarshal(rawBody []byte) (*engine.ChatRequest, error) {
	variant := detectVariant(rawBody)
	switch variant {
	case variantResponses:
		return a.unmarshalResponses(rawBody)
	default:
		return a.unmarshalChat(rawBody)
	}
}

// Marshal converts a ChatRequest back to Chat Completions or Responses wire format.
func (a *Adapter) Marshal(chat *engine.ChatRequest) ([]byte, error) {
	if chat.ProviderExtensions != nil {
		if variant, ok := chat.ProviderExtensions["_openai_variant"].(string); ok && variant == "responses" {
			return marshalResponses(chat)
		}
	}
	return marshalChat(chat)
}

// ---------------------------------------------------------------------------
// variant detection
// ---------------------------------------------------------------------------

type variant int

const (
	variantChat variant = iota
	variantResponses
)

// detectVariant probes the JSON body to determine which API variant it is.
// It returns variantResponses if the body contains "object":"response" or a
// top-level "input" field without "messages". Otherwise it returns
// variantChat.
func detectVariant(raw []byte) variant {
	// Fast heuristic: look for "object":"response" literal.
	if containsKey(raw, `"object":"response"`) || containsKey(raw, `"object": "response"`) {
		return variantResponses
	}
	// Check for top-level "input" without "messages".
	if containsKey(raw, `"input"`) && !containsKey(raw, `"messages"`) {
		return variantResponses
	}
	return variantChat
}

// containsKey does a fast substring check. It is deliberately loose (does not
// parse JSON) because the bodies are small and the patterns are distinctive.
func containsKey(raw []byte, key string) bool {
	return len(raw) > 0 && bytesContains(raw, key)
}

func bytesContains(s []byte, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if string(s[i:i+len(sub)]) == sub {
			return true
		}
	}
	return false
}

func marshalResponses(chat *engine.ChatRequest) ([]byte, error) {
	var rr responseRequest
	rr.Stream = chat.Stream
	if chat.ProviderExtensions != nil {
		if m, ok := chat.ProviderExtensions["_openai_original_model"].(string); ok {
			rr.Model = m
		}
	}

	// Convert messages back to input array if possible.
	if len(chat.Messages) > 0 {
		var msgs []chatMessage
		for _, m := range chat.Messages {
			msgs = append(msgs, chatMessage{
				Role: string(m.Role),
				Content: func() json.RawMessage {
					if len(m.ContentParts) > 0 {
						b, _ := json.Marshal(m.ContentParts)
						return b
					} else if m.Content != "" {
						b, _ := json.Marshal(m.Content)
						return b
					}
					return json.RawMessage(`""`)
				}(),
			})
		}
		b, _ := json.Marshal(msgs)
		rr.Input = b
	}

	for _, t := range chat.Tools {
		rr.Tools = append(rr.Tools, responseTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}

	b, err := json.Marshal(rr)
	if err != nil {
		return nil, err
	}

	if len(chat.ProviderExtensions) > 0 {
		var outMap map[string]any
		json.Unmarshal(b, &outMap)
		for k, v := range chat.ProviderExtensions {
			if !strings.HasPrefix(k, "_openai_") {
				outMap[k] = v
			}
		}
		return json.Marshal(outMap)
	}

	return b, nil
}

// ---------------------------------------------------------------------------
// Chat Completions unmarshal
// ---------------------------------------------------------------------------

func (a *Adapter) unmarshalChat(rawBody []byte) (*engine.ChatRequest, error) {
	var cr chatRequest
	if err := json.Unmarshal(rawBody, &cr); err != nil {
		return nil, fmt.Errorf("openai chat unmarshal: %w", err)
	}

	req := &engine.ChatRequest{
		Model:       cr.Model,
		Stream:      cr.Stream,
		MaxTokens:   cr.MaxTokens,
		Temperature: cr.Temperature,
		TopP:        cr.TopP,
	}

	if cr.StopSequences != nil {
		switch v := cr.StopSequences.(type) {
		case string:
			req.StopSequences = []string{v}
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					req.StopSequences = append(req.StopSequences, s)
				}
			}
		}
	}

	var raw map[string]any
	if err := json.Unmarshal(rawBody, &raw); err == nil {
		delete(raw, "model")
		delete(raw, "messages")
		delete(raw, "tools")
		delete(raw, "stream")
		delete(raw, "max_tokens")
		delete(raw, "temperature")
		delete(raw, "top_p")
		delete(raw, "stop")
		if len(raw) > 0 {
			req.ProviderExtensions = raw
		}
	}

	// Messages.
	for _, m := range cr.Messages {
		msg := convertChatMessage(m)
		req.Messages = append(req.Messages, msg)
	}

	// Tools.
	for _, t := range cr.Tools {
		td := engine.ToolDef{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
			Strict:      t.Strict,
		}
		req.Tools = append(req.Tools, td)
	}

	return req, nil
}
func convertChatMessage(m chatMessage) engine.Message {
	msg := engine.Message{
		Role:       engine.Role(m.Role),
		ToolCallID: m.ToolCallID,
	}

	// Content may be a string or array.
	if len(m.Content) > 0 {
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			msg.Content = s
		} else {
			var parts []any
			if err := json.Unmarshal(m.Content, &parts); err == nil {
				msg.ContentParts = parts
			}
		}
	}

	// Reasoning / thinking content (extended reasoning models).
	if m.ReasoningContent != nil {
		msg.Thinking = *m.ReasoningContent
	}

	// Tool calls.
	for _, tc := range m.ToolCalls {
		args := parseArgs(tc.Function.Arguments)
		msg.ToolCalls = append(msg.ToolCalls, engine.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: args,
		})
	}

	return msg
}

// parseArgs parses a JSON string into map[string]any. On parse failure
// returns an empty map so the pipeline doesn't crash on malformed args.
func parseArgs(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil
	}
	return args
}

// ---------------------------------------------------------------------------
// Responses API unmarshal
// ---------------------------------------------------------------------------

func (a *Adapter) unmarshalResponses(rawBody []byte) (*engine.ChatRequest, error) {
	var rr responseRequest
	if err := json.Unmarshal(rawBody, &rr); err != nil {
		return nil, fmt.Errorf("openai responses unmarshal: %w", err)
	}

	req := &engine.ChatRequest{
		Model:  "gpt-4o",
		Stream: rr.Stream,
	}

	// Preserve the variant and original model in extensions.
	req.ProviderExtensions = map[string]any{
		"_openai_variant": "responses",
	}
	if rr.Model != "" {
		req.ProviderExtensions["_openai_original_model"] = rr.Model
	}

	var raw map[string]any
	if err := json.Unmarshal(rawBody, &raw); err == nil {
		delete(raw, "model")
		delete(raw, "input")
		delete(raw, "tools")
		delete(raw, "stream")
		for k, v := range raw {
			req.ProviderExtensions[k] = v
		}
	}

	// Input: string or array.
	if len(rr.Input) > 0 {
		// Try string first.
		var s string
		if err := json.Unmarshal(rr.Input, &s); err == nil {
			req.Messages = append(req.Messages, engine.Message{
				Role:    engine.RoleUser,
				Content: s,
			})
		} else {
			// Try array of messages.
			var msgs []chatMessage
			if err := json.Unmarshal(rr.Input, &msgs); err == nil {
				for _, m := range msgs {
					req.Messages = append(req.Messages, convertChatMessage(m))
				}
			}
		}
	}

	// Tools (Responses API uses flat tool shape: {type, name, description, parameters}).
	for _, t := range rr.Tools {
		td := engine.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		}
		req.Tools = append(req.Tools, td)
	}

	return req, nil
}

// ---------------------------------------------------------------------------
// Marshal (Chat Completions format)
// ---------------------------------------------------------------------------

// marshalOutput is the Chat Completions JSON shape for marshal.
type marshalOutput struct {
	Model         string        `json:"model"`
	Messages      []marshalMsg  `json:"messages"`
	Tools         []marshalTool `json:"tools,omitempty"`
	Stream        bool          `json:"stream"`
	MaxTokens     *int          `json:"max_tokens,omitempty"`
	Temperature   *float64      `json:"temperature,omitempty"`
	TopP          *float64      `json:"top_p,omitempty"`
	StopSequences []string      `json:"stop,omitempty"`
}

type marshalMsg struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	ToolCalls        []marshalTC     `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
}

type marshalTC struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function marshalTCFn `json:"function"`
}

type marshalTCFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type marshalTool struct {
	Type     string        `json:"type"`
	Function marshalToolFn `json:"function"`
	Strict   bool          `json:"strict,omitempty"`
}

type marshalToolFn struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// modelOrDefault returns m if non-empty, otherwise d.
func modelOrDefault(m, d string) string {
	if m == "" {
		return d
	}
	return m
}
func marshalChat(chat *engine.ChatRequest) ([]byte, error) {
	out := marshalOutput{
		Model:         modelOrDefault(chat.Model, "gpt-4o"),
		Messages:      make([]marshalMsg, 0, len(chat.Messages)),
		Tools:         make([]marshalTool, 0, len(chat.Tools)),
		Stream:        chat.Stream,
		MaxTokens:     chat.MaxTokens,
		Temperature:   chat.Temperature,
		TopP:          chat.TopP,
		StopSequences: chat.StopSequences,
	}

	for _, m := range chat.Messages {
		mm := marshalMsg{
			Role:       string(m.Role),
			ToolCallID: m.ToolCallID,
		}

		// Content: null if empty and there are tool calls (assistant).
		if len(m.ContentParts) > 0 {
			b, _ := json.Marshal(m.ContentParts)
			mm.Content = b
		} else if m.Content != "" {
			b, _ := json.Marshal(m.Content)
			mm.Content = b
		} else if m.Role == engine.RoleAssistant && len(m.ToolCalls) > 0 {
			mm.Content = json.RawMessage("null")
		} else if m.Content == "" {
			mm.Content = json.RawMessage(`""`)
		}

		// Reasoning / thinking content (assistant-only).
		if m.Thinking != "" && m.Role == engine.RoleAssistant {
			mm.ReasoningContent = &m.Thinking
		}

		// Tool calls.
		for _, tc := range m.ToolCalls {
			args := "{}"
			if tc.Arguments != nil {
				b, err := json.Marshal(tc.Arguments)
				if err == nil {
					args = string(b)
				}
			}
			mm.ToolCalls = append(mm.ToolCalls, marshalTC{
				ID:   tc.ID,
				Type: "function",
				Function: marshalTCFn{
					Name:      tc.Name,
					Arguments: args,
				},
			})
		}

		out.Messages = append(out.Messages, mm)
	}

	// Tools.
	for _, t := range chat.Tools {
		out.Tools = append(out.Tools, marshalTool{
			Type:   "function",
			Strict: t.Strict,
			Function: marshalToolFn{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}

	b, err := json.Marshal(out)
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
