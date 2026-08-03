// Package bedrock implements format adapters for AWS Bedrock Converse API.
// Bedrock uses content blocks (text, toolUse, toolResult) similar to Anthropic
// but nests tool definitions under toolConfig.tools[].toolSpec and parameters
// under inputSchema.json.
package bedrock

import (
	"encoding/json"
	"fmt"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
)

func init() {
	format.Register("/bedrock", format.Format{
		Name:    "bedrock",
		Request: &Adapter{},
		Stream:  &Stream{},
	})
}

// Adapter implements format.RequestAdapter for Bedrock Converse.
type Adapter struct{}

// --- Wire types for Bedrock Converse JSON ---

type bedrockRequest struct {
	ModelID         string                  `json:"modelId"`
	System          json.RawMessage         `json:"system"`
	Messages        []bedrockMsg            `json:"messages"`
	ToolConfig      *bedrockToolConfig      `json:"toolConfig,omitempty"`
	InferenceConfig *bedrockInferenceConfig `json:"inferenceConfig,omitempty"`
}

type bedrockMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // array of content blocks
}

type bedrockContentBlock struct {
	Text       *string            `json:"text,omitempty"`
	Thinking   *bedrockThinking   `json:"thinking,omitempty"`
	ToolUse    *bedrockToolUse    `json:"toolUse,omitempty"`
	ToolResult *bedrockToolResult `json:"toolResult,omitempty"`
	// Cache breakpoint block, e.g. {"cachePoint":{"type":"default"}}.
	// Positional: caches the prefix up to this block. Preserved opaquely.
	CachePoint map[string]any `json:"cachePoint,omitempty"`
}

type bedrockThinking struct {
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type bedrockToolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"` // raw lexemes, verbatim
}

type bedrockToolResult struct {
	ToolUseID string `json:"toolUseId"`
	Content   []any  `json:"content"`
}

type bedrockToolConfig struct {
	Tools []bedrockTool `json:"tools"`
}

type bedrockTool struct {
	ToolSpec *bedrockToolSpec `json:"toolSpec,omitempty"`
	// A tools[] entry may instead be a cache breakpoint after the preceding
	// tool definitions: {"cachePoint":{"type":"default"}}.
	CachePoint map[string]any `json:"cachePoint,omitempty"`
}

type bedrockToolSpec struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	InputSchema bedrockSchema `json:"inputSchema"`
}

type bedrockSchema struct {
	JSON json.RawMessage `json:"json"` // raw JSON Schema lexemes, verbatim
}

type bedrockSystemBlock struct {
	Text       string         `json:"text,omitempty"`
	CachePoint map[string]any `json:"cachePoint,omitempty"`
}

type bedrockInferenceConfig struct {
	MaxTokens     *int     `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

// --- Unmarshal ---

func (a *Adapter) Unmarshal(rawBody []byte) (*engine.ChatRequest, error) {
	if len(rawBody) == 0 {
		return nil, fmt.Errorf("bedrock: empty request body")
	}

	var req bedrockRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		return nil, fmt.Errorf("bedrock: unmarshal request: %w", err)
	}

	chat := &engine.ChatRequest{Model: req.ModelID}

	// Parse system blocks: text blocks in wire order with their positional
	// cachePoint elements projected as CacheBreakpoint blocks.
	if len(req.System) > 0 {
		sys, err := parseSystemBlocks(req.System)
		if err != nil {
			return nil, fmt.Errorf("bedrock: unmarshal system: %w", err)
		}
		if len(sys) > 0 {
			chat.Messages = append(chat.Messages, engine.Message{Role: engine.RoleSystem, Blocks: sys})
		}
	}

	// Parse messages: ONE engine message per wire message — provider message
	// boundaries and roles stay intact; tool results ride their user message
	// at their exact position (no synthetic RoleTool split).
	for _, bm := range req.Messages {
		blocks, rawEls, err := parseContentBlocks(bm.Content)
		if err != nil {
			return nil, fmt.Errorf("bedrock: unmarshal message content: %w", err)
		}
		msg, leading, err := contentBlocksToMessage(bm.Role, blocks, rawEls)
		if err != nil {
			return nil, fmt.Errorf("bedrock message: %w", err)
		}
		// A cachePoint before any content in this wire message closes the
		// prefix ending at the PREVIOUS message: attach it as the last block
		// of that message (the covered content).
		if !leadingIsZero(leading) && len(chat.Messages) > 0 {
			prev := &chat.Messages[len(chat.Messages)-1]
			prev.Blocks = append(prev.Blocks, engine.Block{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: leading}})
		}
		chat.Messages = append(chat.Messages, msg)
	}

	// Parse tools: a {"cachePoint":...} entry marks a breakpoint after the
	// preceding tool definitions.
	if req.ToolConfig != nil {
		for _, t := range req.ToolConfig.Tools {
			if t.CachePoint != nil && t.ToolSpec == nil {
				if n := len(chat.Tools); n > 0 {
					cc, cerr := engine.ParseRequiredJSONObject(mustMarshalB(t.CachePoint))
					if cerr != nil {
						return nil, fmt.Errorf("bedrock tool cache_control: %w", cerr)
					}
					chat.Tools[n-1].CacheControl, _ = engine.ParseOptionalJSONObject(cc.Bytes())
				}
				continue
			}
			if t.ToolSpec == nil {
				continue
			}
			params, err := engine.ParseRequiredObjectOrEmpty(t.ToolSpec.InputSchema.JSON)
			if err != nil {
				return nil, fmt.Errorf("tool %q input schema: %w", t.ToolSpec.Name, err)
			}
			chat.Tools = append(chat.Tools, engine.ToolDef{
				Name:        t.ToolSpec.Name,
				Description: t.ToolSpec.Description,
				Parameters:  params,
			})
		}
	}

	if req.InferenceConfig != nil {
		chat.MaxTokens = req.InferenceConfig.MaxTokens
		chat.Temperature = req.InferenceConfig.Temperature
		chat.TopP = req.InferenceConfig.TopP
		chat.StopSequences = req.InferenceConfig.StopSequences
	}

	// Bedrock Converse has no stream field — streaming is a separate API (ConverseStream).
	chat.Stream = false

	// Provider extensions: original body minus the canonical fields, deleted
	// in fixed order (deterministic; unknown members keep lexemes + order).
	ext, xerr := engine.ParseOptionalJSONObject(rawBody)
	if xerr != nil {
		return nil, fmt.Errorf("bedrock provider extensions: %w", xerr)
	}
	ext, xerr = ext.WithoutMembers("modelId", "system", "messages", "toolConfig", "inferenceConfig")
	if xerr != nil {
		return nil, fmt.Errorf("bedrock provider extensions: %w", xerr)
	}
	if ext, xerr = format.NormalizeExtensionObject(ext); xerr != nil {
		return nil, fmt.Errorf("bedrock provider extensions: %w", xerr)
	}
	if !ext.IsAbsent() {
		chat.ProviderExtensions = ext
	}

	return chat, nil
}

// parseSystemBlocks projects the system array onto ordered blocks: text
// blocks in wire order; cachePoint elements become CacheBreakpoint blocks at
// their position. A leading cachePoint (before any text) is preserved at
// position 0 — the canonical positional form.
func parseSystemBlocks(raw json.RawMessage) ([]engine.Block, error) {
	var blocks []bedrockSystemBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	var out []engine.Block
	for _, b := range blocks {
		if b.CachePoint != nil {
			marker, err := engine.ParseRequiredJSONObject(mustMarshalB(b.CachePoint))
			if err != nil {
				return nil, fmt.Errorf("system cachePoint: %w", err)
			}
			out = append(out, engine.Block{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: marker}})
			continue
		}
		out = append(out, engine.Block{Text: &engine.TextBlock{Text: b.Text}})
	}
	return out, nil
}

// parseContentBlocks unmarshals the content JSON as an array of
// bedrockContentBlock, returning the raw element bytes alongside (aligned by
// index) so unknown arms and nested payloads survive the projection
// lexeme-exact.
func parseContentBlocks(raw json.RawMessage) ([]bedrockContentBlock, []json.RawMessage, error) {
	var blocks []bedrockContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, nil, err
	}
	var rawEls []json.RawMessage
	if err := json.Unmarshal(raw, &rawEls); err != nil {
		return nil, nil, err
	}
	return blocks, rawEls, nil
}

// contentBlocksToMessage projects ONE wire message's content array onto
// the ordered body, preserving block order exactly. cachePoint elements are
// positional on BOTH sides: they project directly to CacheBreakpoint blocks
// (a leading cachePoint is returned for the caller to attach to the covered
// previous message).
func contentBlocksToMessage(role string, blocks []bedrockContentBlock, rawEls []json.RawMessage) (engine.Message, engine.RequiredJSONObject, error) {
	msg := engine.Message{Role: mapRole(role)}
	var leading engine.RequiredJSONObject
	for i, b := range blocks {
		switch {
		case b.CachePoint != nil:
			marker, err := engine.ParseRequiredJSONObject(mustMarshalB(b.CachePoint))
			if err != nil {
				return msg, leading, fmt.Errorf("cachePoint: %w", err)
			}
			if len(msg.Blocks) == 0 {
				leading = marker
				continue
			}
			msg.Blocks = append(msg.Blocks, engine.Block{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: marker}})
		case b.Text != nil:
			msg.Blocks = append(msg.Blocks, engine.Block{Text: &engine.TextBlock{Text: *b.Text}})
		case b.Thinking != nil:
			msg.Blocks = append(msg.Blocks, engine.Block{Thinking: &engine.ThinkingBlock{
				Text: b.Thinking.Thinking, Signature: b.Thinking.Signature,
			}})
		case b.ToolUse != nil:
			args, perr := engine.ParseRequiredObjectOrEmpty(b.ToolUse.Input)
			if perr != nil {
				return msg, leading, fmt.Errorf("tool use %q input: %w", b.ToolUse.Name, perr)
			}
			msg.Blocks = append(msg.Blocks, engine.Block{ToolUse: &engine.ToolUseBlock{
				ID: b.ToolUse.ToolUseID, Name: b.ToolUse.Name, Arguments: args,
			}})
		case b.ToolResult != nil:
			tr := &engine.ToolResultBlock{ToolCallID: b.ToolResult.ToolUseID}
			var rawTR struct {
				ToolResult struct {
					Content json.RawMessage `json:"content"`
				} `json:"toolResult"`
			}
			if i < len(rawEls) {
				json.Unmarshal(rawEls[i], &rawTR)
			}
			var partsRaw []json.RawMessage
			json.Unmarshal(rawTR.ToolResult.Content, &partsRaw)
			for j, part := range b.ToolResult.Content {
				elem, eerr := bedrockNestedToEngine(part, partsRaw, j)
				if eerr != nil {
					return msg, leading, eerr
				}
				tr.Content = append(tr.Content, elem)
			}
			if len(tr.Content) == 0 {
				tr.Content = []engine.ToolResultContentBlock{{Text: ""}}
			}
			msg.Blocks = append(msg.Blocks, engine.Block{ToolResult: tr})
		default:
			// image / document / json / any unmodelled arm: raw preservation
			// with the discriminant removed (the projection invariant).
			kind := "unknown"
			raw := json.RawMessage(nil)
			if i < len(rawEls) {
				raw = rawEls[i]
				if k := firstMemberName(raw); k != "" {
					kind = k
				}
			}
			payload, perr := stripBedrockFacts(raw)
			if perr != nil {
				return msg, leading, fmt.Errorf("block %q payload: %w", kind, perr)
			}
			msg.Blocks = append(msg.Blocks, engine.Block{Unknown: &engine.UnknownBlock{Kind: kind, Payload: payload}})
		}
	}
	return msg, leading, nil
}

// bedrockNestedToEngine projects one tool-result content element onto the
// nested kinds (text / unknown). rawNested holds the block's raw content
// array aligned by index.
func bedrockNestedToEngine(part any, rawNested []json.RawMessage, j int) (engine.ToolResultContentBlock, error) {
	if m, ok := part.(map[string]any); ok && len(m) == 1 {
		if txt, ok := m["text"].(string); ok {
			return engine.ToolResultContentBlock{Text: txt}, nil
		}
	}
	kind := "unknown"
	if m, ok := part.(map[string]any); ok {
		for k := range m {
			if k != "text" {
				kind = k
				break
			}
		}
	}
	raw := json.RawMessage(nil)
	if j < len(rawNested) {
		raw = rawNested[j]
	}
	payload, err := stripBedrockFacts(raw)
	if err != nil {
		return engine.ToolResultContentBlock{}, fmt.Errorf("nested block %q payload: %w", kind, err)
	}
	return engine.ToolResultContentBlock{Unknown: &engine.UnknownBlock{Kind: kind, Payload: payload}}, nil
}

// firstMemberName returns the first member name of a raw object (the
// bedrock block discriminant, e.g. "image"/"document"/"json").
func firstMemberName(raw json.RawMessage) string {
	if len(raw) == 0 || raw[0] != '{' {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for k := range m {
		return k
	}
	return ""
}

// stripBedrockFacts unwraps a raw bedrock block object: the discriminant
// IS the member name ("image"/"document"/"json"), so the payload is the
// member's VALUE, projected span-preserving. A missing raw element is a
// refusal (the adapter's raw capture is authoritative).
func stripBedrockFacts(raw json.RawMessage) (engine.RequiredJSONObject, error) {
	if len(raw) == 0 || raw[0] != '{' {
		return engine.RequiredJSONObject{}, fmt.Errorf("expected a JSON object block")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return engine.RequiredJSONObject{}, err
	}
	for k, v := range m {
		if k == "text" {
			continue // text arms are handled before this helper
		}
		val := v
		if len(val) == 0 || val[0] != '{' {
			return engine.RequiredJSONObject{}, fmt.Errorf("discriminant %q payload: expected a JSON object", k)
		}
		return engine.ParseRequiredJSONObject(val)
	}
	return engine.RequiredJSONObject{}, fmt.Errorf("expected a JSON object block")
}

// leadingIsZero reports whether a leading marker was never captured (the
// zero RequiredJSONObject is the canonical {} — indistinguishable from a
// genuinely empty marker object, which bedrock would not emit).
func leadingIsZero(m engine.RequiredJSONObject) bool {
	return string(m.Bytes()) == "{}"
}

// mustMarshalB encodes a wire-decoded map back to bytes (cache markers).
func mustMarshalB(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func mapRole(role string) engine.Role {
	switch role {
	case "user":
		return engine.RoleUser
	case "assistant":
		return engine.RoleAssistant
	default:
		return engine.RoleUser
	}
}

func (a *Adapter) Marshal(chat *engine.ChatRequest) ([]byte, error) {
	modelID := "anthropic.claude-sonnet-4-20250514-v1:0"
	if chat.Model != "" {
		modelID = chat.Model
	}
	req := &bedrockRequest{
		ModelID: modelID,
	}

	// System message
	for _, m := range chat.Messages {
		if m.Role == engine.RoleSystem {
			sys, err := marshalSystemBlocks(m)
			if err != nil {
				return nil, err
			}
			sysRaw, merr := json.Marshal(sys)
			if merr != nil {
				return nil, merr
			}
			req.System = sysRaw
			break // only first system message
		}
	}

	// Messages (excluding system)
	for _, m := range chat.Messages {
		if m.Role == engine.RoleSystem {
			continue
		}
		if m.Role == engine.RoleTool {
			return nil, fmt.Errorf("bedrock: tool-role messages are not representable; " +
				"tool results must ride their user message as ToolResult blocks")
		}
		bm, err := marshalMessage(m)
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, bm)
	}

	// Tools
	if len(chat.Tools) > 0 {
		req.ToolConfig = &bedrockToolConfig{}
		for _, td := range chat.Tools {
			req.ToolConfig.Tools = append(req.ToolConfig.Tools, bedrockTool{
				ToolSpec: &bedrockToolSpec{
					Name:        td.Name,
					Description: td.Description,
					InputSchema: bedrockSchema{
						JSON: td.Parameters.Bytes(),
					},
				},
			})
			if !td.CacheControl.IsAbsent() {
				req.ToolConfig.Tools = append(req.ToolConfig.Tools, bedrockTool{
					CachePoint: decodeMapB(td.CacheControl),
				})
			}
		}
	}

	if chat.MaxTokens != nil || chat.Temperature != nil || chat.TopP != nil || len(chat.StopSequences) > 0 {
		req.InferenceConfig = &bedrockInferenceConfig{
			MaxTokens:     chat.MaxTokens,
			Temperature:   chat.Temperature,
			TopP:          chat.TopP,
			StopSequences: chat.StopSequences,
		}
	}

	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	if !chat.ProviderExtensions.IsAbsent() {
		var outMap map[string]json.RawMessage
		if err := json.Unmarshal(b, &outMap); err != nil {
			return nil, err
		}
		if err := format.MergeRawMembers(outMap, chat.ProviderExtensions.Bytes()); err != nil {
			return nil, fmt.Errorf("bedrock provider extensions merge: %w", err)
		}
		return json.Marshal(outMap)
	}

	return b, nil
}

// marshalSystemBlocks projects a system message's ordered body onto the
// system array: text blocks in wire order; CacheBreakpoint blocks become
// cachePoint ELEMENTS at their position (bedrock markers are positional on
// both sides). Any other kind fails closed.
func marshalSystemBlocks(m engine.Message) ([]bedrockSystemBlock, error) {
	var out []bedrockSystemBlock
	for i, b := range m.Blocks {
		switch {
		case b.Text != nil:
			out = append(out, bedrockSystemBlock{Text: b.Text.Text})
		case b.CacheBreakpoint != nil:
			out = append(out, bedrockSystemBlock{CachePoint: decodeMapB2(b.CacheBreakpoint.Marker)})
		default:
			return nil, fmt.Errorf("bedrock: system block %d (%s) is not representable on the system array",
				i, blockKindB(b))
		}
	}
	return out, nil
}

// marshalMessage projects one message's ordered body onto the wire content
// array, enforcing the Bedrock provider grammar fail-closed: cachePoint
// elements are positional; a trailing signature, redacted thinking, and
// tool-role messages are unrepresentable; tool_use is assistant-only and
// tool_result is user-only.
func marshalMessage(m engine.Message) (bedrockMsg, error) {
	bm := bedrockMsg{Role: reverseRole(m.Role)}
	var blocks []bedrockContentBlock
	for i, b := range m.Blocks {
		var cb bedrockContentBlock
		switch {
		case b.Text != nil:
			text := b.Text.Text
			cb = bedrockContentBlock{Text: &text}
		case b.Thinking != nil:
			cb = bedrockContentBlock{Thinking: &bedrockThinking{
				Thinking: b.Thinking.Text, Signature: b.Thinking.Signature,
			}}
		case b.ToolUse != nil:
			if m.Role != engine.RoleAssistant {
				return bedrockMsg{}, fmt.Errorf("bedrock: toolUse block %d on a %q message is unrepresentable", i, m.Role)
			}
			cb = bedrockContentBlock{ToolUse: &bedrockToolUse{
				ToolUseID: b.ToolUse.ID, Name: b.ToolUse.Name, Input: b.ToolUse.Arguments.Bytes(),
			}}
		case b.ToolResult != nil:
			if m.Role != engine.RoleUser {
				return bedrockMsg{}, fmt.Errorf("bedrock: toolResult block %d on a %q message is unrepresentable", i, m.Role)
			}
			content, cerr := marshalNestedB(b.ToolResult.Content)
			if cerr != nil {
				return bedrockMsg{}, cerr
			}
			cb = bedrockContentBlock{ToolResult: &bedrockToolResult{
				ToolUseID: b.ToolResult.ToolCallID, Content: content,
			}}
		case b.CacheBreakpoint != nil:
			cb = bedrockContentBlock{CachePoint: decodeMapB2(b.CacheBreakpoint.Marker)}
		case b.Unknown != nil:
			raw, uerr := reattachDiscriminantB(b.Unknown)
			if uerr != nil {
				return bedrockMsg{}, fmt.Errorf("bedrock: %w", uerr)
			}
			if err := json.Unmarshal(raw, &cb); err != nil {
				return bedrockMsg{}, fmt.Errorf("bedrock: unknown block: %w", err)
			}
		case b.RedactedThinking != nil:
			return bedrockMsg{}, fmt.Errorf("bedrock: redacted_thinking block %d is not representable", i)
		case b.TrailingSignature != nil:
			return bedrockMsg{}, fmt.Errorf("bedrock: trailing signature block %d is not representable (Code Assist only)", i)
		default:
			return bedrockMsg{}, fmt.Errorf("bedrock: block %d has no arm", i)
		}
		blocks = append(blocks, cb)
	}
	raw, merr := json.Marshal(blocks)
	if merr != nil {
		return bedrockMsg{}, fmt.Errorf("bedrock message blocks: %w", merr)
	}
	bm.Content = raw
	return bm, nil
}

// marshalNestedB projects nested tool-result content onto the wire array
// (text blocks and unknown arms with their discriminants re-attached).
func marshalNestedB(content []engine.ToolResultContentBlock) ([]any, error) {
	if len(content) == 0 {
		return []any{}, nil
	}
	var out []any
	for _, c := range content {
		switch {
		case c.Unknown != nil:
			if c.Unknown.Kind == "text" {
				return nil, fmt.Errorf("bedrock: nested unknown block kind %q names a modeled arm (projection invariant)", c.Unknown.Kind)
			}
			payload, _, err := c.Unknown.Payload.DecodeObject()
			if err != nil {
				return nil, fmt.Errorf("nested unknown payload: %w", err)
			}
			block := make(map[string]any, len(payload)+1)
			block[c.Unknown.Kind] = map[string]json.RawMessage(payload)
			out = append(out, block)
		case c.CacheBreakpoint != nil:
			// Bedrock does not permit cachePoint inside toolResult content:
			// fail closed rather than drop.
			return nil, fmt.Errorf("bedrock: nested cache breakpoints are not representable in toolResult content")
		default:
			out = append(out, map[string]any{"text": c.Text})
		}
	}
	return out, nil
}

// reattachDiscriminantB re-attaches the unknown block's kind as the
// single-member discriminant ({"image": {...}}).
func reattachDiscriminantB(u *engine.UnknownBlock) ([]byte, error) {
	// The projection invariant: the kind IS the discriminant — a kind that
	// names a modeled arm would fabricate a wire block the verifier never
	// saw, so it is rejected before marshal.
	switch u.Kind {
	case "text", "toolUse", "toolResult":
		return nil, fmt.Errorf("bedrock: unknown block kind %q names a modeled arm (projection invariant)", u.Kind)
	}
	payload, _, err := u.Payload.DecodeObject()
	if err != nil {
		return nil, fmt.Errorf("unknown payload: %w", err)
	}
	block := map[string]any{u.Kind: map[string]json.RawMessage(payload)}
	return json.Marshal(block)
}

// decodeMapB2 decodes a required marker for the wire map shape.
func decodeMapB2(m engine.RequiredJSONObject) map[string]any {
	var out map[string]any
	if err := json.Unmarshal(m.Bytes(), &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// decodeMapB decodes an optional marker for the wire map shape.
func decodeMapB(o engine.OptionalJSONObject) map[string]any {
	if o.IsAbsent() {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(o.Bytes(), &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// blockKindB names a block for error messages.
func blockKindB(b engine.Block) string {
	switch {
	case b.Text != nil:
		return "text"
	case b.Thinking != nil:
		return "thinking"
	case b.RedactedThinking != nil:
		return "redacted_thinking"
	case b.ToolUse != nil:
		return "tool_use"
	case b.ToolResult != nil:
		return "tool_result"
	case b.CacheBreakpoint != nil:
		return "cache_breakpoint"
	case b.Unknown != nil:
		return "unknown"
	case b.TrailingSignature != nil:
		return "trailing_signature"
	default:
		return "empty"
	}
}
func reverseRole(role engine.Role) string {
	switch role {
	case engine.RoleAssistant:
		return "assistant"
	case engine.RoleTool:
		return "user" // tool results are sent as user messages in Bedrock
	case engine.RoleSystem:
		return "system"
	default:
		return "user"
	}
}
