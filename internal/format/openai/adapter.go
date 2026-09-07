// Package openai implements format adapters for OpenAI-compatible APIs.
// It handles both the Chat Completions API and the Responses API, detecting
// which variant is in use from the JSON body structure.
package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
	"github.com/torana-edge/torana-edge/internal/format"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
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

// VerifyResponsesToolTopologyPB keeps provider namespace membership bound to
// the accepted Responses request. Plugins may rewrite a definition under
// ir.tools.write, but namespace membership and the number/order of namespaced
// leaves are provider topology, not a schema mutation. Top-level tools remain
// freely addable/removable under the normal tools grant.
func VerifyResponsesToolTopologyPB(current, replacement *pb.ChatRequest) error {
	paths := func(req *pb.ChatRequest) [][]string {
		var out [][]string
		if req == nil {
			return out
		}
		for _, tool := range req.Tools {
			if tool != nil && len(tool.NamespacePath) != 0 {
				out = append(out, append([]string(nil), tool.NamespacePath...))
			}
		}
		return out
	}
	before, after := paths(current), paths(replacement)
	if len(before) != len(after) {
		return fmt.Errorf("openai responses: namespaced tool topology is host-owned")
	}
	for i := range before {
		if !slices.Equal(before[i], after[i]) {
			return fmt.Errorf("openai responses: namespaced tool %d moved between provider namespaces", i)
		}
	}
	return nil
}

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
	// Name is the tool name a tool-role message may carry beside its
	// tool_call_id. The IR has always had a slot for it
	// (ToolResultBlock.ToolName); this adapter simply never read it.
	Name string `json:"name,omitempty"`
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
}

type chatToolFuncDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // raw JSON Schema lexemes
	// Strict lives INSIDE function on Chat Completions. It sat at the tool
	// level here — copied from the Responses shape, where flat is correct — so
	// a caller's strict schema was dropped on the way in and re-emitted where
	// the provider ignores it. Structured outputs silently stopped being
	// strict for anything routed through Torana.
	Strict bool `json:"strict,omitempty"`
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
	Format      json.RawMessage `json:"format,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

type responsesInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Input     *string         `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
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
	if err := format.RejectFreeformTools(chat, "openai chat"); err != nil {
		return nil, err
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
		items, lerr := responsesItemsWithLayout(projected, chat.ResponsesInputLayout.Bytes(), chat.Tools)
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
		if len(t.NamespacePath) != 0 {
			continue
		}
		wt := responseTool{Name: t.Name, Description: t.Description, Strict: t.Strict}
		if t.InvocationKind == engine.ToolInvocationFreeform {
			wt.Type = "custom"
			wt.Format = t.InputFormat.Bytes()
		} else {
			wt.Type = "function"
			wt.Parameters = t.Parameters.Bytes()
		}
		rr.Tools = append(rr.Tools, wt)
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
func responsesItemsWithLayout(projected []any, layout []byte, tools []engine.ToolDef) ([]any, error) {
	var rawItems []json.RawMessage
	if err := json.Unmarshal(layout, &rawItems); err != nil {
		return nil, fmt.Errorf("openai responses layout: %w", err)
	}
	items := make([]any, 0, len(rawItems))
	mi := 0
	namespaced := make([]engine.ToolDef, 0)
	for _, tool := range tools {
		if len(tool.NamespacePath) != 0 {
			namespaced = append(namespaced, tool)
		}
	}
	ni := 0
	for _, ri := range rawItems {
		var t struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(ri, &t); err != nil {
			return nil, fmt.Errorf("openai responses layout item: %w", err)
		}
		switch t.Type {
		case "message", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output":
			if mi >= len(projected) {
				return nil, fmt.Errorf("openai responses: layout has more representable items than messages")
			}
			merged, err := mergeProjectedResponseItem(ri, projected[mi])
			if err != nil {
				return nil, err
			}
			items = append(items, merged)
			mi++
		case "additional_tools":
			rebuilt, consumed, err := rebuildAdditionalTools(ri, namespaced[ni:])
			if err != nil {
				return nil, err
			}
			items = append(items, rebuilt)
			ni += consumed
		default:
			// Opaque item: verbatim (the host-only topology).
			items = append(items, ri)
		}
	}
	if mi < len(projected) {
		return nil, fmt.Errorf("openai responses: %d projected item(s) have no layout slot", len(projected)-mi)
	}
	if ni != len(namespaced) {
		return nil, fmt.Errorf("openai responses: %d namespaced tool(s) have no additional_tools layout slot", len(namespaced)-ni)
	}
	return items, nil
}

func mergeProjectedResponseItem(original json.RawMessage, projected any) (json.RawMessage, error) {
	var out map[string]json.RawMessage
	if err := json.Unmarshal(original, &out); err != nil {
		return nil, fmt.Errorf("openai responses layout item: %w", err)
	}
	canonicalKeys := []string{"type", "role", "content", "name", "arguments", "call_id", "output", "input"}
	b, err := json.Marshal(projected)
	if err != nil {
		return nil, fmt.Errorf("openai responses projected item: %w", err)
	}
	var canonical map[string]json.RawMessage
	if err := json.Unmarshal(b, &canonical); err != nil {
		return nil, err
	}
	// The layout is the lossless authority for provider item details. If the
	// canonical projection is unchanged, retain the original item rather than
	// routing it through a map and changing member order or raw lexemes.
	unchanged := true
	for _, key := range canonicalKeys {
		before, beforeOK := out[key]
		after, afterOK := canonical[key]
		if beforeOK != afterOK || (beforeOK && !sameJSONValue(before, after)) {
			unchanged = false
			break
		}
	}
	if unchanged {
		return append(json.RawMessage(nil), original...), nil
	}
	for _, key := range canonicalKeys {
		delete(out, key)
	}
	for key, value := range canonical {
		out[key] = value
	}
	return json.Marshal(out)
}

func sameJSONValue(a, b []byte) bool {
	decode := func(raw []byte) (any, error) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var out any
		if err := dec.Decode(&out); err != nil {
			return nil, err
		}
		if err := dec.Decode(&struct{}{}); err != io.EOF {
			return nil, fmt.Errorf("trailing JSON")
		}
		return out, nil
	}
	left, err := decode(a)
	if err != nil {
		return false
	}
	right, err := decode(b)
	return err == nil && reflect.DeepEqual(left, right)
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
				if b.ToolResult.InvocationKind == engine.ToolInvocationFreeform {
					output, err := customToolOutputFromEngine(b.ToolResult.Content)
					if err != nil {
						return nil, err
					}
					items = append(items, map[string]any{"type": "custom_tool_call_output", "call_id": b.ToolResult.ToolCallID, "output": output})
					continue
				}
				var text string
				for _, c := range b.ToolResult.Content {
					if c.Unknown != nil || c.CacheBreakpoint != nil {
						return nil, fmt.Errorf("openai responses: structured tool-result content is not representable")
					}
					text += c.Text
				}
				items = append(items, map[string]any{"type": "function_call_output", "call_id": b.ToolResult.ToolCallID, "output": text})
			}
		case engine.RoleAssistant:
			for _, b := range m.Blocks {
				switch {
				case b.ToolUse != nil:
					if b.ToolUse.InvocationKind == engine.ToolInvocationFreeform {
						if b.ToolUse.InputText == nil {
							return nil, fmt.Errorf("openai responses: free-form tool call has no input")
						}
						items = append(items, map[string]any{"type": "custom_tool_call", "call_id": b.ToolUse.ID, "name": b.ToolUse.Name, "input": *b.ToolUse.InputText})
					} else {
						items = append(items, map[string]any{
							"type": "function_call", "call_id": b.ToolUse.ID,
							"name": b.ToolUse.Name, "arguments": b.ToolUse.Arguments.String(),
						})
					}
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

func customToolOutputFromEngine(content []engine.ToolResultContentBlock) ([]any, error) {
	out := make([]any, 0, len(content))
	for i, c := range content {
		switch {
		case c.CacheBreakpoint != nil:
			return nil, fmt.Errorf("openai responses: custom tool output[%d] cache marker is not representable", i)
		case c.Unknown != nil:
			if err := rejectOpenAIProjection(c.Unknown); err != nil {
				return nil, err
			}
			payload, _, err := c.Unknown.Payload.DecodeObject()
			if err != nil {
				return nil, fmt.Errorf("custom tool output[%d]: %w", i, err)
			}
			item := make(map[string]any, len(payload)+1)
			item["type"] = c.Unknown.Kind
			for k, v := range payload {
				item[k] = json.RawMessage(v)
			}
			out = append(out, item)
		default:
			out = append(out, map[string]any{"type": "input_text", "text": c.Text})
		}
	}
	return out, nil
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
		// Silently dropping an element rewrote the caller's stop sequences —
		// the model then runs past a boundary the caller set. A shape this
		// adapter cannot carry is refused, not quietly narrowed.
		switch v := cr.StopSequences.(type) {
		case string:
			req.StopSequences = []string{v}
		case []any:
			for i, item := range v {
				text, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("openai chat: stop[%d] must be a string", i)
				}
				req.StopSequences = append(req.StopSequences, text)
			}
		default:
			return nil, fmt.Errorf("openai chat: stop must be a string or an array of strings")
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
			Strict:      t.Function.Strict,
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
		content, cerr := chatToolResultContent(m.Content)
		if cerr != nil {
			return msg, cerr
		}
		result := engine.Block{ToolResult: &engine.ToolResultBlock{
			ToolCallID: m.ToolCallID,
			ToolName:   m.Name,
			Content:    content,
		}}
		// The tool result replaces the CONTENT blocks, not the whole body.
		// Assigning over msg.Blocks discarded two things silently: array-form
		// content (only the scalar arm was read below, so the result reached
		// the provider empty) and any reasoning block built above.
		kept := make([]engine.Block, 0, len(msg.Blocks)+1)
		for _, b := range msg.Blocks {
			if b.Thinking != nil {
				kept = append(kept, b)
			}
		}
		msg.Blocks = append(kept, result)
	}

	// A message the wire carried but that projects to no block — `{"role":
	// "assistant"}`, or content: [] — is a valid provider shape. The IR spells
	// an empty message as one explicit empty text block; leaving the list empty
	// instead tripped the SDK's "at least one block" rule as a 400 the caller
	// could do nothing about.
	if len(msg.Blocks) == 0 {
		msg.Blocks = []engine.Block{{Text: &engine.TextBlock{Text: ""}}}
	}

	return msg, nil
}

// chatToolResultContent projects a tool message's content onto the ordered
// nested content of a tool result, accepting both wire arms. A present-but-
// empty array becomes the canonical one-empty-text-element spelling.
func chatToolResultContent(c chatContent) ([]engine.ToolResultContentBlock, error) {
	if !c.Present || c.Text != nil {
		text := ""
		if c.Text != nil {
			text = *c.Text
		}
		return []engine.ToolResultContentBlock{{Text: text}}, nil
	}
	out := make([]engine.ToolResultContentBlock, 0, len(c.Parts))
	for i, p := range c.Parts {
		elem, err := openAIPartToToolResultContent(p)
		if err != nil {
			return nil, fmt.Errorf("tool result content[%d]: %w", i, err)
		}
		out = append(out, elem)
	}
	if len(out) == 0 {
		return []engine.ToolResultContentBlock{{Text: ""}}, nil
	}
	return out, nil
}

// openAIPartToToolResultContent is openAIPartToBlock for the nested tool-result
// kinds: text elements become text, anything else keeps its discriminant and
// raw payload as an unknown element.
func openAIPartToToolResultContent(p json.RawMessage) (engine.ToolResultContentBlock, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(p, &probe); err != nil {
		return engine.ToolResultContentBlock{}, fmt.Errorf("content part: %w", err)
	}
	switch probe.Type {
	case "text":
		var t struct {
			Text *string `json:"text"`
		}
		if err := json.Unmarshal(p, &t); err != nil {
			return engine.ToolResultContentBlock{}, fmt.Errorf("content part: %w", err)
		}
		if t.Text == nil {
			return engine.ToolResultContentBlock{}, fmt.Errorf("openai chat: text part without a text member")
		}
		return engine.ToolResultContentBlock{Text: *t.Text}, nil
	case "":
		return engine.ToolResultContentBlock{}, fmt.Errorf("openai chat: content part without a type member")
	default:
		payload, err := stripOpenAIPartFacts(p, "type")
		if err != nil {
			return engine.ToolResultContentBlock{}, fmt.Errorf("content part %q payload: %w", probe.Type, err)
		}
		return engine.ToolResultContentBlock{Unknown: &engine.UnknownBlock{Kind: probe.Type, Payload: payload}}, nil
	}
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
		var output string
		if err := json.Unmarshal(item.Output, &output); err != nil {
			return engine.Message{}, fmt.Errorf("function call output: %w", err)
		}
		return engine.Message{
			Role: engine.RoleTool,
			Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{
				ToolCallID: item.CallID,
				Content:    []engine.ToolResultContentBlock{{Text: output}},
			}}},
		}, nil
	case "custom_tool_call":
		if item.CallID == "" || item.Name == "" || item.Input == nil {
			return engine.Message{}, fmt.Errorf("custom tool call requires call_id, name, and input")
		}
		input := *item.Input
		return engine.Message{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{
			ID: item.CallID, Name: item.Name, InputText: &input, InvocationKind: engine.ToolInvocationFreeform,
		}}}}, nil
	case "custom_tool_call_output":
		content, err := customToolOutputToEngine(item.Output)
		if err != nil {
			return engine.Message{}, err
		}
		return engine.Message{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{
			ToolCallID: item.CallID, InvocationKind: engine.ToolInvocationFreeform, Content: content,
		}}}}, nil
	default:
		// Unmodelled item kinds are refused: the wire item would be dropped
		// or mis-rendered, and a plugin cannot see what it cannot represent.
		return engine.Message{}, fmt.Errorf("responses input item %q is not representable", item.Type)
	}
}

func customToolOutputToEngine(raw json.RawMessage) ([]engine.ToolResultContentBlock, error) {
	var scalar string
	if err := json.Unmarshal(raw, &scalar); err == nil {
		return []engine.ToolResultContentBlock{{Text: scalar}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return nil, fmt.Errorf("custom tool output must be a string or non-empty array")
	}
	out := make([]engine.ToolResultContentBlock, 0, len(items))
	for i, item := range items {
		var textValue string
		if err := json.Unmarshal(item, &textValue); err == nil {
			out = append(out, engine.ToolResultContentBlock{Text: textValue})
			continue
		}
		var probe struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			return nil, fmt.Errorf("custom tool output[%d]: %w", i, err)
		}
		if probe.Type == "input_text" {
			if probe.Text == nil {
				return nil, fmt.Errorf("custom tool output[%d]: input_text requires text", i)
			}
			out = append(out, engine.ToolResultContentBlock{Text: *probe.Text})
			continue
		}
		if probe.Type == "" {
			return nil, fmt.Errorf("custom tool output[%d]: type is required", i)
		}
		payload, err := stripOpenAIPartFacts(item, "type")
		if err != nil {
			return nil, fmt.Errorf("custom tool output[%d]: %w", i, err)
		}
		out = append(out, engine.ToolResultContentBlock{Unknown: &engine.UnknownBlock{Kind: probe.Type, Payload: payload}})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Responses API unmarshal
// ---------------------------------------------------------------------------

func (a *Adapter) unmarshalResponses(rawBody []byte) (*engine.ChatRequest, error) {
	var rr responseRequest
	if err := json.Unmarshal(rawBody, &rr); err != nil {
		return nil, fmt.Errorf("openai responses unmarshal: %w", err)
	}

	// No default model. Substituting one sends a different, differently-priced
	// request than the caller wrote and misattributes every downstream cost and
	// metric; refusing would break the single-model local endpoints in
	// docs/LOCAL_MODELS.md, which accept a request without one. Forward what
	// the caller sent — marshalOutput omits an empty model.
	req := &engine.ChatRequest{
		Model:  rr.Model,
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
			var rawItems []json.RawMessage
			if err := json.Unmarshal(rr.Input, &rawItems); err == nil && len(rawItems) > 0 && responseItemType(rawItems[0]) != "" {
				// Representable kinds project onto the ordered body; opaque
				// kinds (reasoning, compaction, future types) are host-only
				// topology captured raw in the layout ext and re-spliced on
				// marshal — never dropped, never guessed. Decode each known
				// item independently: an opaque item is allowed to use a field
				// name with a different JSON type (Codex custom_tool_call_output,
				// for example, has an array-valued output), which must not make
				// decoding the entire heterogeneous array fail.
				sawNonToolItem := false
				for i, rawItem := range rawItems {
					typ := responseItemType(rawItem)
					if typ == "additional_tools" {
						if sawNonToolItem {
							return nil, fmt.Errorf("openai responses input item %d: additional_tools must precede conversation items", i)
						}
						defs, derr := additionalToolsToEngine(rawItem)
						if derr != nil {
							return nil, fmt.Errorf("openai responses input item %d: %w", i, derr)
						}
						req.Tools = append(req.Tools, defs...)
						continue
					}
					sawNonToolItem = true
					if typ != "message" && typ != "function_call" && typ != "function_call_output" &&
						typ != "custom_tool_call" && typ != "custom_tool_call_output" {
						continue
					}
					var item responsesInputItem
					if err := json.Unmarshal(rawItem, &item); err != nil {
						return nil, fmt.Errorf("openai responses input item %d: %w", i, err)
					}
					msg, merr := responsesItemToMessage(item)
					if merr != nil {
						return nil, fmt.Errorf("openai responses input item %d: %w", i, merr)
					}
					req.Messages = append(req.Messages, msg)
				}
				layout, lerr := engine.ParseOptionalJSONArray(rr.Input)
				if lerr != nil {
					return nil, fmt.Errorf("openai responses input layout: %w", lerr)
				}
				req.ResponsesInputLayout = layout
			} else {
				// Try legacy array of messages. A decode failure here used to
				// be discarded, leaving zero messages and re-emitting the
				// caller's input as null — the conversation destroyed with no
				// error to anybody.
				var msgs []chatMessage
				if err := json.Unmarshal(rr.Input, &msgs); err != nil {
					return nil, fmt.Errorf("openai responses: input must be a string, an item array, or a message array: %w", err)
				}
				for _, m := range msgs {
					msg, merr := convertChatMessage(m)
					if merr != nil {
						return nil, merr
					}
					req.Messages = append(req.Messages, msg)
				}
			}
		}
	}

	// The input branch may have extended the host-only extensions (the
	// opaque layout); carry the updated value onto the request.
	req.ProviderExtensions = ext

	// Tools (Responses API uses flat tool shape: {type, name, description, parameters}).
	topLevelTools := make([]engine.ToolDef, 0, len(rr.Tools))
	for _, t := range rr.Tools {
		td, err := responseToolToEngine(t, nil)
		if err != nil {
			return nil, err
		}
		topLevelTools = append(topLevelTools, td)
	}
	req.Tools = append(topLevelTools, req.Tools...)

	return req, nil
}

func responseToolToEngine(t responseTool, namespace []string) (engine.ToolDef, error) {
	td := engine.ToolDef{Name: t.Name, Description: t.Description, Strict: t.Strict, NamespacePath: append([]string(nil), namespace...)}
	switch t.Type {
	case "", "function":
		params, err := engine.ParseRequiredObjectOrEmpty(t.Parameters)
		if err != nil {
			return td, fmt.Errorf("tool %q parameters: %w", t.Name, err)
		}
		td.Parameters = params
	case "custom":
		format, err := engine.ParseRequiredJSONObject(t.Format)
		if err != nil {
			return td, fmt.Errorf("tool %q format: %w", t.Name, err)
		}
		td.InvocationKind = engine.ToolInvocationFreeform
		td.InputFormat = format
	default:
		return td, fmt.Errorf("tool %q has unsupported type %q", t.Name, t.Type)
	}
	return td, nil
}

func additionalToolsToEngine(raw json.RawMessage) ([]engine.ToolDef, error) {
	var root struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("additional_tools: %w", err)
	}
	var out []engine.ToolDef
	for i, child := range root.Tools {
		if err := walkAdditionalTool(child, nil, &out); err != nil {
			return nil, fmt.Errorf("additional_tools.tools[%d]: %w", i, err)
		}
	}
	return out, nil
}

func walkAdditionalTool(raw json.RawMessage, path []string, out *[]engine.ToolDef) error {
	var probe struct {
		Type        string            `json:"type"`
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Parameters  json.RawMessage   `json:"parameters"`
		Format      json.RawMessage   `json:"format"`
		Strict      bool              `json:"strict"`
		Tools       []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	if probe.Type == "namespace" {
		if probe.Name == "" || len(probe.Tools) == 0 {
			return fmt.Errorf("namespace requires a name and non-empty tools")
		}
		next := append(append([]string(nil), path...), probe.Name)
		for i, child := range probe.Tools {
			if err := walkAdditionalTool(child, next, out); err != nil {
				return fmt.Errorf("namespace %q tools[%d]: %w", probe.Name, i, err)
			}
		}
		return nil
	}
	td, err := responseToolToEngine(responseTool{
		Type: probe.Type, Name: probe.Name, Description: probe.Description,
		Parameters: probe.Parameters, Format: probe.Format, Strict: probe.Strict,
	}, path)
	if err != nil {
		return err
	}
	*out = append(*out, td)
	return nil
}

func rebuildAdditionalTools(raw json.RawMessage, tools []engine.ToolDef) (json.RawMessage, int, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, 0, fmt.Errorf("openai responses additional_tools: %w", err)
	}
	var children []json.RawMessage
	if err := json.Unmarshal(root["tools"], &children); err != nil {
		return nil, 0, fmt.Errorf("openai responses additional_tools.tools: %w", err)
	}
	consumed := 0
	for i, child := range children {
		rebuilt, err := rebuildAdditionalTool(child, nil, tools, &consumed)
		if err != nil {
			return nil, 0, fmt.Errorf("openai responses additional_tools.tools[%d]: %w", i, err)
		}
		children[i] = rebuilt
	}
	b, err := json.Marshal(children)
	if err != nil {
		return nil, 0, err
	}
	root["tools"] = b
	out, err := json.Marshal(root)
	return out, consumed, err
}

func rebuildAdditionalTool(raw json.RawMessage, path []string, tools []engine.ToolDef, consumed *int) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	var typ, name string
	if err := json.Unmarshal(obj["type"], &typ); err != nil {
		return nil, fmt.Errorf("type: %w", err)
	}
	if err := json.Unmarshal(obj["name"], &name); err != nil {
		return nil, fmt.Errorf("name: %w", err)
	}
	if typ == "namespace" {
		var children []json.RawMessage
		if err := json.Unmarshal(obj["tools"], &children); err != nil {
			return nil, fmt.Errorf("namespace %q tools: %w", name, err)
		}
		next := append(append([]string(nil), path...), name)
		for i, child := range children {
			rebuilt, err := rebuildAdditionalTool(child, next, tools, consumed)
			if err != nil {
				return nil, err
			}
			children[i] = rebuilt
		}
		b, err := json.Marshal(children)
		if err != nil {
			return nil, err
		}
		obj["tools"] = b
		return json.Marshal(obj)
	}
	if *consumed >= len(tools) {
		return nil, fmt.Errorf("layout has more namespaced tool leaves than the canonical request")
	}
	td := tools[*consumed]
	if !slices.Equal(td.NamespacePath, path) {
		return nil, fmt.Errorf("tool %q namespace changed from %v to %v; additional_tools topology mutation is unsupported", td.Name, path, td.NamespacePath)
	}
	*consumed++
	obj["name"], _ = json.Marshal(td.Name)
	obj["description"], _ = json.Marshal(td.Description)
	if td.InvocationKind == engine.ToolInvocationFreeform {
		obj["type"] = json.RawMessage(`"custom"`)
		obj["format"] = td.InputFormat.Bytes()
		delete(obj, "parameters")
		delete(obj, "strict")
	} else {
		obj["type"] = json.RawMessage(`"function"`)
		obj["parameters"] = td.Parameters.Bytes()
		obj["strict"], _ = json.Marshal(td.Strict)
		delete(obj, "format")
	}
	return json.Marshal(obj)
}

func responseItemType(raw json.RawMessage) string {
	var item struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return ""
	}
	return item.Type
}

// ---------------------------------------------------------------------------
// Marshal (Chat Completions format)
// ---------------------------------------------------------------------------

// marshalOutput is the Chat Completions JSON shape for marshal.
type marshalOutput struct {
	Model         string        `json:"model,omitempty"`
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
	Name             string      `json:"name,omitempty"`
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
}

type marshalToolFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // raw JSON Schema lexemes
	// Inside function, per the Chat Completions schema. See chatToolFuncDef.
	Strict bool `json:"strict,omitempty"`
}

func marshalChat(chat *engine.ChatRequest) ([]byte, error) {
	out := marshalOutput{
		Model:         chat.Model,
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
			Type: "function",
			Function: marshalToolFn{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters.Bytes(),
				Strict:      t.Strict,
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
// chatContentPart is one item of an OpenAI chat message's content array, held
// in the order its block was visited. isText/text are set only for parts this
// adapter built from a text block, so the single-text scalar-string form can be
// recognised without inspecting the marshalled value (an unknown block whose
// discriminant happens to be "text" is NOT that form).
type chatContentPart struct {
	value  any
	text   string
	isText bool
}

func textContentPart(s string) chatContentPart {
	return chatContentPart{
		value:  map[string]any{"type": "text", "text": s},
		text:   s,
		isText: true,
	}
}

func marshalChatMessage(m engine.Message) (marshalMsg, error) {
	mm := marshalMsg{Role: string(m.Role)}
	// ONE ordered projection, appended to as blocks are visited. Text and
	// unknown parts used to accumulate in separate buckets that were
	// concatenated text-first at the end, so every mixed message came back
	// reordered — [image, text] marshalled as [text, image] — on both the
	// tool-result path and the ordinary content array.
	var parts []chatContentPart
	for i, b := range m.Blocks {
		switch {
		case b.Text != nil:
			parts = append(parts, textContentPart(b.Text.Text))
		case b.Thinking != nil:
			// reasoning_content is a passthrough vendor member on this wire,
			// not modeled thinking with provenance rules, so it mirrors the
			// message it arrived on rather than being assistant-only. The
			// previous restriction was unreachable: the tool-role branch in
			// convertChatMessage discarded the block before marshal ever saw
			// it, which is exactly the silent loss being fixed here. Echoing
			// back what the caller sent is what a transparent proxy owes them;
			// if the provider rejects the member, it would have rejected the
			// caller's original request too.
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
			mm.Name = b.ToolResult.ToolName
			for _, c := range b.ToolResult.Content {
				switch {
				case c.Unknown != nil:
					// Representable: the content-array branch below emits it
					// with its discriminant restored, the same as any other
					// unknown part. Refusing here meant a tool result the
					// caller sent could not be sent back.
					if err := rejectOpenAIProjection(c.Unknown); err != nil {
						return mm, err
					}
					payload, _, err := c.Unknown.Payload.DecodeObject()
					if err != nil {
						return mm, fmt.Errorf("unknown tool-result payload: %w", err)
					}
					block := make(map[string]any, len(payload)+1)
					block["type"] = c.Unknown.Kind
					for k, v := range payload {
						block[k] = json.RawMessage(v)
					}
					parts = append(parts, chatContentPart{value: block})
				case c.CacheBreakpoint != nil:
					return mm, fmt.Errorf("openai chat: nested cache breakpoints are not representable")
				default:
					parts = append(parts, textContentPart(c.Text))
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
			parts = append(parts, chatContentPart{value: block})
		case b.RedactedThinking != nil:
			return mm, fmt.Errorf("openai chat: redacted_thinking block %d is not representable", i)
		case b.TrailingSignature != nil:
			return mm, fmt.Errorf("openai chat: trailing signature block %d is not representable (Code Assist only)", i)
		default:
			return mm, fmt.Errorf("openai chat: block %d has no arm", i)
		}
	}

	// Content: the scalar-string form applies ONLY when the ordered projection
	// is exactly one text part. Everything else — a lone unknown part, any mix,
	// any repetition — emits the content ARRAY in visit order.
	switch {
	case len(parts) > 1, len(parts) == 1 && !parts[0].isText:
		content := make([]any, 0, len(parts))
		for _, p := range parts {
			content = append(content, p.value)
		}
		mm.Content = content
	case len(parts) == 1 && parts[0].text != "":
		mm.Content = parts[0].text
	case m.Role == engine.RoleAssistant && len(mm.ToolCalls) > 0:
		mm.Content = json.RawMessage("null")
	case len(parts) == 1:
		mm.Content = ""
	}
	return mm, nil
}
