// Package openai implements format adapters for OpenAI-compatible APIs.
// It handles both the Chat Completions API and the Responses API, detecting
// which variant is in use from the JSON body structure.
package openai

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format"
)

func init() {
	format.Register("/openai", format.Format{
		Name:             "openai",
		Request:          &Adapter{},
		Stream:           &StreamAdapter{},
		MatchesInference: format.PostInferencePaths("/chat/completions", "/responses"),
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
	Role             string         `json:"role"`
	Content          chatContent    `json:"content,omitempty"`
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

// chatContent decodes scalar content directly into its durable string. A
// json.RawMessage first copied the complete value and convertChatMessage then
// decoded that copy again; coding-agent prompts make this the largest field in
// the request. Structured content retains raw elements because each arm still
// needs independent provider-grammar projection.
type chatContent struct {
	Text    *string
	Parts   []json.RawMessage
	Present bool
}

func (c *chatContent) UnmarshalJSON(raw []byte) error {
	*c = chatContent{Present: true}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		c.Text = &text
		return nil
	}
	if err := json.Unmarshal(raw, &c.Parts); err != nil {
		return err
	}
	return nil
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
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // raw JSON Schema lexemes
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
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // raw JSON Schema lexemes
}

type responsesInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Output    string          `json:"output,omitempty"`
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
	// The owning validation at EVERY marshal entry: the engine pointer sum
	// must be in the closed domain before any arm is projected — a future
	// call site cannot bypass the checked boundary by accident.
	if err := pbconv.ValidateFullRequest(chat); err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	// The typed host-only topology fact decides the wire variant; a plugin
	// can neither forge nor lose it.
	if chat.OpenAIVariant == engine.OpenAIResponses {
		return marshalResponses(chat)
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

type jsonPresence bool

func (p *jsonPresence) UnmarshalJSON([]byte) error {
	*p = true
	return nil
}

// detectVariant decides which OpenAI API variant a body belongs to.
//
// It decodes rather than scanning for substrings. The scan it replaces was
// unanchored, so it could not tell a top-level key from the same text inside a
// message: a Responses request whose prompt merely contained the characters
// "messages" — a coding agent pasting a request body, say, which is exactly
// Torana's traffic — was routed to the Chat parser and mis-parsed. The keys
// that decide this are top-level, so the check must be too.
//
// Only the three deciding keys are bound; everything else stays raw.
func detectVariant(raw []byte) variant {
	var probe struct {
		Object   string       `json:"object"`
		Input    jsonPresence `json:"input"`
		Messages jsonPresence `json:"messages"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		// Not a decodable JSON object. Chat is the historical default, and the
		// parser reports its own error more usefully than a guess here would.
		return variantChat
	}
	if probe.Object == "response" {
		return variantResponses
	}
	if probe.Input && !probe.Messages {
		return variantResponses
	}
	return variantChat
}

func marshalResponses(chat *engine.ChatRequest) ([]byte, error) {
	var rr responseRequest
	rr.Stream = chat.Stream
	rr.Model = chat.Model

	// Convert messages back to input items in wire order. When the caller's
	// layout was captured (unmarshal), opaque items (reasoning, compaction,
	// future types) are re-spliced at their recorded positions verbatim;
	// representable slots take the projected (possibly plugin-mutated)
	// items. Without a captured layout the ordered body IS the layout.
	projected, err := responsesItemsFromMessages(chat.Messages)
	if err != nil {
		return nil, err
	}
	if !chat.ResponsesInputLayout.IsAbsent() {
		items, lerr := responsesItemsWithLayout(projected, chat.ResponsesInputLayout.Bytes())
		if lerr != nil {
			return nil, lerr
		}
		b, merr := json.Marshal(items)
		if merr != nil {
			return nil, fmt.Errorf("openai responses items: %w", merr)
		}
		rr.Input = b
	} else if len(projected) > 0 {
		b, merr := json.Marshal(projected)
		if merr != nil {
			return nil, fmt.Errorf("openai responses items: %w", merr)
		}
		rr.Input = b
	}

	for _, t := range chat.Tools {
		rr.Tools = append(rr.Tools, responseTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters.Bytes(),
		})
	}

	b, err := json.Marshal(rr)
	if err != nil {
		return nil, err
	}

	if !chat.ProviderExtensions.IsAbsent() {
		var outMap map[string]json.RawMessage
		if err := json.Unmarshal(b, &outMap); err != nil {
			return nil, err
		}
		if err := format.MergeRawMembersFiltered(outMap, chat.ProviderExtensions.Bytes(), func(k string) bool {
			return !strings.HasPrefix(k, "_openai_")
		}); err != nil {
			return nil, fmt.Errorf("openai provider extensions merge: %w", err)
		}
		return json.Marshal(outMap)
	}

	return b, nil
}

// rejectOpenAIProjection enforces the projection invariant before marshal:
// an unknown block's payload must not duplicate the canonical "type"
// discriminant (which would silently override the kind), and the kind must
// not name a modeled arm (which would fabricate a wire block the verifier
// never saw).
func rejectOpenAIProjection(u *engine.UnknownBlock) error {
	payload, _, err := u.Payload.DecodeObject()
	if err != nil {
		return fmt.Errorf("unknown payload: %w", err)
	}
	if _, dup := payload["type"]; dup {
		return fmt.Errorf("openai: unknown payload duplicates canonical member %q (projection invariant)", "type")
	}
	switch u.Kind {
	case "text", "tool_calls", "tool_call":
		return fmt.Errorf("openai: unknown block kind %q names a modeled arm (projection invariant)", u.Kind)
	}
	return nil
}

// responsesItemsWithLayout splices the projected items into the captured
// input layout: representable layout slots (message / function_call /
// function_call_output) take the projected items in order; opaque slots are
// re-emitted verbatim. Any count mismatch is a refusal — a layout that no
// longer matches the body would silently drop or duplicate items.
func responsesItemsWithLayout(projected []any, layout []byte) ([]any, error) {
	var rawItems []json.RawMessage
	if err := json.Unmarshal(layout, &rawItems); err != nil {
		return nil, fmt.Errorf("openai responses layout: %w", err)
	}
	items := make([]any, 0, len(rawItems))
	mi := 0
	for _, ri := range rawItems {
		var t struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(ri, &t); err != nil {
			return nil, fmt.Errorf("openai responses layout item: %w", err)
		}
		switch t.Type {
		case "message", "function_call", "function_call_output":
			if mi >= len(projected) {
				return nil, fmt.Errorf("openai responses: layout has more representable items than messages")
			}
			items = append(items, projected[mi])
			mi++
		default:
			// Opaque item: verbatim (the host-only topology).
			items = append(items, ri)
		}
	}
	if mi < len(projected) {
		return nil, fmt.Errorf("openai responses: %d projected item(s) have no layout slot", len(projected)-mi)
	}
	return items, nil
}

// responsesItemsFromMessages projects messages onto Responses input items in
// wire order: message items for text/unknown-bearing messages, function_call
// items for tool_use blocks, function_call_output items for tool-role
// messages. Unrepresentable block kinds fail closed.
func responsesItemsFromMessages(messages []engine.Message) ([]any, error) {
	var items []any
	for _, m := range messages {
		switch m.Role {
		case engine.RoleTool:
			for _, b := range m.Blocks {
				if b.ToolResult == nil {
					return nil, fmt.Errorf("openai responses: tool-role message with a non-tool-result block")
				}
				var text string
				for _, c := range b.ToolResult.Content {
					if c.Unknown != nil || c.CacheBreakpoint != nil {
						return nil, fmt.Errorf("openai responses: structured tool-result content is not representable")
					}
					text += c.Text
				}
				items = append(items, map[string]any{
					"type":    "function_call_output",
					"call_id": b.ToolResult.ToolCallID,
					"output":  text,
				})
			}
		case engine.RoleAssistant:
			for _, b := range m.Blocks {
				switch {
				case b.ToolUse != nil:
					items = append(items, map[string]any{
						"type":      "function_call",
						"call_id":   b.ToolUse.ID,
						"name":      b.ToolUse.Name,
						"arguments": b.ToolUse.Arguments.String(),
					})
				case b.Text != nil:
					items = append(items, messageItem(string(m.Role), b.Text.Text))
				case b.Unknown != nil:
					if err := rejectOpenAIProjection(b.Unknown); err != nil {
						return nil, err
					}
					payload, _, err := b.Unknown.Payload.DecodeObject()
					if err != nil {
						return nil, fmt.Errorf("unknown payload: %w", err)
					}
					block := make(map[string]any, len(payload)+1)
					block["type"] = b.Unknown.Kind
					for k, v := range payload {
						block[k] = json.RawMessage(v)
					}
					items = append(items, messageItem(string(m.Role), block))
				case b.Thinking != nil:
					// Responses has no reasoning item in the request input;
					// fail closed rather than drop.
					return nil, fmt.Errorf("openai responses: thinking blocks are not representable in input items")
				default:
					return nil, fmt.Errorf("openai responses: block kind not representable in input items")
				}
			}
		default:
			for _, b := range m.Blocks {
				switch {
				case b.Text != nil:
					items = append(items, messageItem(string(m.Role), b.Text.Text))
				case b.Unknown != nil:
					if err := rejectOpenAIProjection(b.Unknown); err != nil {
						return nil, err
					}
					payload, _, err := b.Unknown.Payload.DecodeObject()
					if err != nil {
						return nil, fmt.Errorf("unknown payload: %w", err)
					}
					block := make(map[string]any, len(payload)+1)
					block["type"] = b.Unknown.Kind
					for k, v := range payload {
						block[k] = json.RawMessage(v)
					}
					items = append(items, messageItem(string(m.Role), block))
				default:
					return nil, fmt.Errorf("openai responses: %s message with a non-text/non-unknown block", m.Role)
				}
			}
		}
	}
	return items, nil
}

// messageItem builds a Responses message input item.
func messageItem(role string, content any) map[string]any {
	return map[string]any{
		"type":    "message",
		"role":    role,
		"content": content,
	}
}

// ---------------------------------------------------------------------------
// Chat Completions unmarshal
// ---------------------------------------------------------------------------

func (a *Adapter) unmarshalChat(rawBody []byte) (*engine.ChatRequest, error) {
	var cr chatRequest
	if err := json.Unmarshal(rawBody, &cr); err != nil {
		return nil, fmt.Errorf("openai chat unmarshal: %w", err)
	}

	if cr.MaxTokens != nil && (*cr.MaxTokens < 1 || *cr.MaxTokens > math.MaxInt32) {
		return nil, fmt.Errorf("openai chat: max_tokens %d is outside 1..%d", *cr.MaxTokens, math.MaxInt32)
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

	// Provider extensions: original body minus the canonical fields, deleted
	// in fixed order (deterministic; unknown members keep lexemes + order).
	ext, xerr := engine.ParseOptionalJSONObjectExcluding(rawBody,
		"model", "messages", "tools", "stream",
		"max_tokens", "temperature", "top_p", "stop")
	if xerr != nil {
		return nil, fmt.Errorf("openai provider extensions: %w", xerr)
	}
	if ext, xerr = format.NormalizeExtensionObject(ext); xerr != nil {
		return nil, fmt.Errorf("openai provider extensions: %w", xerr)
	}
	if !ext.IsAbsent() {
		req.ProviderExtensions = ext
	}

	// Messages.
	for _, m := range cr.Messages {
		msg, err := convertChatMessage(m)
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, msg)
	}

	// Tools.
	for _, t := range cr.Tools {
		params, err := engine.ParseRequiredObjectOrEmpty(t.Function.Parameters)
		if err != nil {
			return nil, fmt.Errorf("tool %q parameters: %w", t.Function.Name, err)
		}
		td := engine.ToolDef{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  params,
			Strict:      t.Strict,
		}
		req.Tools = append(req.Tools, td)
	}

	return req, nil
}

func convertChatMessage(m chatMessage) (engine.Message, error) {
	msg := engine.Message{Role: engine.Role(m.Role)}

	// Content may be a string or array; array parts project to Text or
	// Unknown blocks in wire order (empty text parts are first-class).
	if m.Content.Present {
		if m.Content.Text != nil {
			msg.Blocks = append(msg.Blocks, engine.Block{Text: &engine.TextBlock{Text: *m.Content.Text}})
		} else {
			for _, p := range m.Content.Parts {
				blk, berr := openAIPartToBlock(p)
				if berr != nil {
					return msg, berr
				}
				msg.Blocks = append(msg.Blocks, blk)
			}
		}
	}

	// Reasoning / thinking content (extended reasoning models).
	if m.ReasoningContent != nil {
		msg.Blocks = append(msg.Blocks, engine.Block{Thinking: &engine.ThinkingBlock{Text: *m.ReasoningContent}})
	}

	// Tool calls (assistant).
	for _, tc := range m.ToolCalls {
		args, err := engine.ParseRequiredObjectOrEmpty([]byte(tc.Function.Arguments))
		if err != nil {
			return msg, fmt.Errorf("tool call %q arguments: %w", tc.Function.Name, err)
		}
		msg.Blocks = append(msg.Blocks, engine.Block{ToolUse: &engine.ToolUseBlock{
			ID: tc.ID, Name: tc.Function.Name, Arguments: args,
		}})
	}

	// Tool-role messages carry one tool result (the chat wire's native tool
	// message shape).
	if m.Role == "tool" {
		if m.ToolCallID == "" {
			return msg, fmt.Errorf("tool message missing tool_call_id")
		}
		var text string
		if m.Content.Text != nil {
			text = *m.Content.Text
		}
		msg.Blocks = []engine.Block{{
			ToolResult: &engine.ToolResultBlock{
				ToolCallID: m.ToolCallID,
				Content:    []engine.ToolResultContentBlock{{Text: text}},
			},
		}}
	}

	return msg, nil
}

// openAIPartToBlock projects one chat content-array part: text parts become
// Text blocks (explicit empty preserved); anything else becomes an Unknown
// block with the discriminant ("type") removed from the payload.
func openAIPartToBlock(p json.RawMessage) (engine.Block, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(p, &probe); err != nil {
		return engine.Block{}, fmt.Errorf("content part: %w", err)
	}
	if probe.Type == "text" {
		// The provider-arm matrix: a DECLARED text arm must actually carry
		// its required member — {"type":"text"} with no text member is
		// malformed, not an implicit empty text (the explicit empty text is
		// spelled {"type":"text","text":""}). A part with NO type member is
		// outside the normative grammar entirely — no legacy form survives
		// merely because the old parser accepted it.
		var t struct {
			Text *string `json:"text"`
		}
		if err := json.Unmarshal(p, &t); err != nil {
			return engine.Block{}, fmt.Errorf("content part: %w", err)
		}
		if t.Text == nil {
			return engine.Block{}, fmt.Errorf("openai chat: text part without a text member")
		}
		return engine.Block{Text: &engine.TextBlock{Text: *t.Text}}, nil
	}
	if probe.Type == "" {
		return engine.Block{}, fmt.Errorf("openai chat: content part without a type member")
	}
	payload, err := stripOpenAIPartFacts(p, "type")
	if err != nil {
		return engine.Block{}, fmt.Errorf("content part %q payload: %w", probe.Type, err)
	}
	return engine.Block{Unknown: &engine.UnknownBlock{Kind: probe.Type, Payload: payload}}, nil
}

// stripOpenAIPartFacts removes the discriminant member from a raw part
// object (span-preserving).
func stripOpenAIPartFacts(raw json.RawMessage, keys ...string) (engine.RequiredJSONObject, error) {
	if len(raw) == 0 || raw[0] != '{' {
		return engine.RequiredJSONObject{}, fmt.Errorf("expected a JSON object part")
	}
	obj, err := engine.ParseRequiredJSONObject(raw)
	if err != nil {
		return obj, err
	}
	for _, k := range keys {
		obj, err = obj.DeleteMember(k)
		if err != nil {
			return obj, err
		}
	}
	return obj, nil
}

// mustParseToolArgs is the Responses unmarshal site for tool-call
// arguments: the caller's arguments field is JSON text (decoded once at the
// parse boundary — the documented canonicalization); empty text normalizes
// to the canonical `{}`.
func mustParseToolArgs(raw string) (engine.RequiredJSONObject, error) {
	return engine.ParseRequiredObjectOrEmpty([]byte(raw))
}

// responsesItemToMessage projects one Responses input item onto the ordered
// body.
func responsesItemToMessage(item responsesInputItem) (engine.Message, error) {
	switch item.Type {
	case "message":
		msg := engine.Message{Role: engine.Role(item.Role)}
		if len(item.Content) > 0 {
			var cs string
			if err := json.Unmarshal(item.Content, &cs); err == nil {
				msg.Blocks = []engine.Block{{Text: &engine.TextBlock{Text: cs}}}
				return msg, nil
			}
			var parts []json.RawMessage
			if err := json.Unmarshal(item.Content, &parts); err == nil {
				for _, p := range parts {
					blk, berr := openAIPartToBlock(p)
					if berr != nil {
						return msg, berr
					}
					msg.Blocks = append(msg.Blocks, blk)
				}
			}
		}
		return msg, nil
	case "function_call":
		args, err := mustParseToolArgs(item.Arguments)
		if err != nil {
			return engine.Message{}, fmt.Errorf("tool call %q arguments: %w", item.Name, err)
		}
		return engine.Message{
			Role: engine.RoleAssistant,
			Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{
				ID: item.CallID, Name: item.Name, Arguments: args,
			}}},
		}, nil
	case "function_call_output":
		return engine.Message{
			Role: engine.RoleTool,
			Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{
				ToolCallID: item.CallID,
				Content:    []engine.ToolResultContentBlock{{Text: item.Output}},
			}}},
		}, nil
	default:
		// Unmodelled item kinds are refused: the wire item would be dropped
		// or mis-rendered, and a plugin cannot see what it cannot represent.
		return engine.Message{}, fmt.Errorf("responses input item %q is not representable", item.Type)
	}
}

// ---------------------------------------------------------------------------
// Responses API unmarshal
// ---------------------------------------------------------------------------

func (a *Adapter) unmarshalResponses(rawBody []byte) (*engine.ChatRequest, error) {
	var rr responseRequest
	if err := json.Unmarshal(rawBody, &rr); err != nil {
		return nil, fmt.Errorf("openai responses unmarshal: %w", err)
	}

	model := rr.Model
	if model == "" {
		model = "gpt-4o"
	}

	req := &engine.ChatRequest{
		Model:  model,
		Stream: rr.Stream,
	}

	// Host-only topology: the variant + original model sentinels stay in the
	// provider extensions (the typed host-state commit replaces them). The
	// input LAYOUT is the ordered body itself — item order is block order,
	// so the old layout sentinel is gone.
	ext, xerr := engine.ParseOptionalJSONObjectExcluding(rawBody,
		"model", "input", "tools", "stream")
	if xerr != nil {
		return nil, fmt.Errorf("openai responses provider extensions: %w", xerr)
	}
	// The variant is typed host-only topology, never an ABI sentinel.
	req.OpenAIVariant = engine.OpenAIResponses
	req.ProviderExtensions = ext

	// Input: string or array.
	if len(rr.Input) > 0 {
		// Try string first.
		var s string
		if err := json.Unmarshal(rr.Input, &s); err == nil {
			req.Messages = append(req.Messages, engine.Message{
				Role:   engine.RoleUser,
				Blocks: []engine.Block{{Text: &engine.TextBlock{Text: s}}},
			})
		} else {
			// Try array of Responses API items: each item projects onto the
			// ordered body in wire order — message boundaries and item order
			// ARE the body (no layout sentinel needed).
			var items []responsesInputItem
			if err := json.Unmarshal(rr.Input, &items); err == nil && len(items) > 0 && items[0].Type != "" {
				// Representable kinds project onto the ordered body; opaque
				// kinds (reasoning, compaction, future types) are host-only
				// topology captured raw in the layout ext and re-spliced on
				// marshal — never dropped, never guessed.
				for _, item := range items {
					msg, merr := responsesItemToMessage(item)
					if merr == nil {
						req.Messages = append(req.Messages, msg)
					}
				}
				layout, lerr := engine.ParseOptionalJSONArray(rr.Input)
				if lerr != nil {
					return nil, fmt.Errorf("openai responses input layout: %w", lerr)
				}
				req.ResponsesInputLayout = layout
			} else {
				// Try legacy array of messages.
				var msgs []chatMessage
				if err := json.Unmarshal(rr.Input, &msgs); err == nil {
					for _, m := range msgs {
						msg, err := convertChatMessage(m)
						if err != nil {
							return nil, err
						}
						req.Messages = append(req.Messages, msg)
					}
				}
			}
		}
	}

	// The input branch may have extended the host-only extensions (the
	// opaque layout); carry the updated value onto the request.
	req.ProviderExtensions = ext

	// Tools (Responses API uses flat tool shape: {type, name, description, parameters}).
	for _, t := range rr.Tools {
		params, err := engine.ParseRequiredObjectOrEmpty(t.Parameters)
		if err != nil {
			return nil, fmt.Errorf("tool %q parameters: %w", t.Name, err)
		}
		td := engine.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
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
	Role             string      `json:"role"`
	Content          any         `json:"content,omitempty"`
	ReasoningContent *string     `json:"reasoning_content,omitempty"`
	ToolCalls        []marshalTC `json:"tool_calls,omitempty"`
	ToolCallID       string      `json:"tool_call_id,omitempty"`
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
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // raw JSON Schema lexemes
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
		mm, err := marshalChatMessage(m)
		if err != nil {
			return nil, err
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
				Parameters:  t.Parameters.Bytes(),
			},
		})
	}

	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}

	if !chat.ProviderExtensions.IsAbsent() {
		var outMap map[string]json.RawMessage
		if err := json.Unmarshal(b, &outMap); err != nil {
			return nil, err
		}
		if err := format.MergeRawMembers(outMap, chat.ProviderExtensions.Bytes()); err != nil {
			return nil, fmt.Errorf("openai provider extensions merge: %w", err)
		}
		return json.Marshal(outMap)
	}

	return b, nil
}

// marshalChatMessage projects one message's ordered body onto the Chat
// Completions shape, enforcing the provider grammar fail-closed: cache
// breakpoints, trailing signatures, and redacted thinking are
// unrepresentable; tool_use is assistant-only; tool results ride the native
// tool-role message shape.
func marshalChatMessage(m engine.Message) (marshalMsg, error) {
	mm := marshalMsg{Role: string(m.Role)}
	var textParts []string
	var unknownParts []any
	for i, b := range m.Blocks {
		switch {
		case b.Text != nil:
			textParts = append(textParts, b.Text.Text)
		case b.Thinking != nil:
			if m.Role != engine.RoleAssistant {
				return mm, fmt.Errorf("openai chat: thinking block %d on a %q message is unrepresentable", i, m.Role)
			}
			mm.ReasoningContent = &b.Thinking.Text
		case b.ToolUse != nil:
			if m.Role != engine.RoleAssistant {
				return mm, fmt.Errorf("openai chat: tool_use block %d on a %q message is unrepresentable", i, m.Role)
			}
			mm.ToolCalls = append(mm.ToolCalls, marshalTC{
				ID:   b.ToolUse.ID,
				Type: "function",
				Function: marshalTCFn{
					Name:      b.ToolUse.Name,
					Arguments: b.ToolUse.Arguments.String(),
				},
			})
		case b.ToolResult != nil:
			if m.Role != engine.RoleTool {
				return mm, fmt.Errorf("openai chat: tool_result block %d on a %q message is unrepresentable "+
					"(the native tool-role message is the only representable carrier)", i, m.Role)
			}
			mm.ToolCallID = b.ToolResult.ToolCallID
			for _, c := range b.ToolResult.Content {
				switch {
				case c.Unknown != nil:
					return mm, fmt.Errorf("openai chat: structured tool-result content is not representable")
				case c.CacheBreakpoint != nil:
					return mm, fmt.Errorf("openai chat: nested cache breakpoints are not representable")
				default:
					textParts = append(textParts, c.Text)
				}
			}
		case b.CacheBreakpoint != nil:
			return mm, fmt.Errorf("openai chat: cache breakpoint block %d is not representable", i)
		case b.Unknown != nil:
			if err := rejectOpenAIProjection(b.Unknown); err != nil {
				return mm, err
			}
			payload, _, err := b.Unknown.Payload.DecodeObject()
			if err != nil {
				return mm, fmt.Errorf("unknown payload: %w", err)
			}
			block := make(map[string]any, len(payload)+1)
			block["type"] = b.Unknown.Kind
			for k, v := range payload {
				block[k] = json.RawMessage(v)
			}
			unknownParts = append(unknownParts, block)
		case b.RedactedThinking != nil:
			return mm, fmt.Errorf("openai chat: redacted_thinking block %d is not representable", i)
		case b.TrailingSignature != nil:
			return mm, fmt.Errorf("openai chat: trailing signature block %d is not representable (Code Assist only)", i)
		default:
			return mm, fmt.Errorf("openai chat: block %d has no arm", i)
		}
	}

	// Content: a single text part emits a string; multiple/empty text parts
	// or unknown parts emit a content ARRAY preserving positions.
	switch {
	case len(unknownParts) > 0 || len(textParts) > 1:
		content := make([]any, 0, len(textParts)+len(unknownParts))
		for _, t := range textParts {
			content = append(content, map[string]any{"type": "text", "text": t})
		}
		content = append(content, unknownParts...)
		mm.Content = content
	case len(textParts) == 1 && textParts[0] != "":
		mm.Content = textParts[0]
	case m.Role == engine.RoleAssistant && len(mm.ToolCalls) > 0:
		mm.Content = json.RawMessage("null")
	case len(textParts) == 1:
		mm.Content = ""
	}
	return mm, nil
}
