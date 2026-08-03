// Package anthropic implements format.RequestAdapter for the Anthropic Messages API.
package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
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
	System        []contentBlock     `json:"system,omitempty"` // string or array; string canonicalizes to one text block
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicToolDef `json:"tools,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	StopReason    string             `json:"-"`
}

type anthropicMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
	// ContentRaw holds the raw "content" member bytes (unmarshal side only;
	// never serialized) so block lexemes survive the ordered-body projection.
	ContentRaw json.RawMessage `json:"-"`
}

// UnmarshalJSON accepts BOTH valid Anthropic system forms: a bare string
// (canonicalized to ONE text block, per the approved parse-bypass seam) and
// the content-block array (existing semantics incl. cache breakpoints).
func (ar *anthropicRequest) UnmarshalJSON(data []byte) error {
	type alias anthropicRequest
	var raw struct {
		alias
		System json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*ar = anthropicRequest(raw.alias)
	if raw.System == nil {
		return nil
	}
	// The Anthropic system union is string | Array<TextBlockParam> — NOT
	// nullable. An explicit null is a parse error like any other wrong type
	// (json.Unmarshal(null, &s) would otherwise succeed leaving s empty and
	// silently treat the null as absent).
	if bytes.Equal(bytes.TrimSpace(raw.System), []byte("null")) {
		return fmt.Errorf("system: expected string or array")
	}

	// Try string first: canonicalize to one text block. An empty string
	// yields an empty text block, which the system coalescing drops, so
	// `"system": ""` behaves like an absent system.
	var s string
	if json.Unmarshal(raw.System, &s) == nil {
		ar.System = []contentBlock{{Type: "text", Text: s}}
		return nil
	}

	// Try array: every element must be a TextBlockParam. The official arm is
	// Array<TextBlockParam>, NOT arbitrary contentBlock — a null element, an
	// empty object, a missing `type`, a non-`text` type, or a missing `text`
	// is a parse error (never silently dropped, which would weaken the
	// system prompt the pipeline inspected). The supported member inventory
	// is type, text, cache_control; any other member (e.g. citations) is not
	// representable by Torana's IR and is rejected with the same value-free
	// error rather than silently discarded. Empty text is distinct from
	// missing text: `{"type":"text","text":""}` is a valid empty block
	// (absent-like after coalescing), `{"type":"text"}` is not.
	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(raw.System, &rawBlocks); err != nil {
		return fmt.Errorf("system: expected string or array: %w", err)
	}
	blocks := make([]contentBlock, 0, len(rawBlocks))
	for i, rb := range rawBlocks {
		if bytes.Equal(bytes.TrimSpace(rb), []byte("null")) {
			return fmt.Errorf("system: element %d: expected a text block", i)
		}
		var el struct {
			Type         string         `json:"type"`
			Text         *string        `json:"text"`
			CacheControl map[string]any `json:"cache_control"`
		}
		dec := json.NewDecoder(bytes.NewReader(rb))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&el); err != nil {
			return fmt.Errorf("system: element %d: %w", i, err)
		}
		if el.Type != "text" || el.Text == nil {
			return fmt.Errorf("system: element %d: expected a text block with a text member", i)
		}
		blocks = append(blocks, contentBlock{
			Type:         "text",
			Text:         *el.Text,
			CacheControl: el.CacheControl,
		})
	}
	ar.System = blocks
	return nil
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
	am.ContentRaw = append(json.RawMessage(nil), raw.Content...)

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
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"` // raw lexemes, verbatim
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"` // string or array of blocks
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	// Cache breakpoint marker, e.g. {"type":"ephemeral"}. Preserved verbatim
	// (opaque map) so TTL variants pass through. Dropping it disables the
	// provider's prompt cache for the whole prefix.
	CacheControl map[string]any `json:"cache_control,omitempty"`
	// Also handle tool_result content as array of blocks (Anthropic supports both)
}

type anthropicToolDef struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"` // raw JSON Schema lexemes, verbatim
	CacheControl map[string]any  `json:"cache_control,omitempty"`
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
	// The provider-arm matrix: a DECLARED anthropic arm must carry its
	// required members — a text block without a text member, a tool_use
	// without id/name, or a tool_result without tool_use_id is malformed,
	// not an implicit default. (Explicit empty text is spelled
	// {"type":"text","text":""}; a MISSING tool_use input is the approved
	// nil->{} normalization, not an error — a present-but-invalid input is
	// refused by the required-object constructor.)
	switch raw.Type {
	case anthropicText:
		var t struct {
			Text *string `json:"text"`
		}
		if json.Unmarshal(data, &t) == nil && t.Text == nil {
			return fmt.Errorf("anthropic: text block without a text member")
		}
		// The provider-arm matrix: a DECLARED arm may not carry members of
		// another arm — a text block with tool-use identity/input would be
		// a cross-arm fact the switch would silently drop.
		if raw.ID != "" || raw.Name != "" || len(raw.Input) > 0 || raw.ToolUseID != "" || raw.Thinking != "" || raw.Data != "" || raw.Content != nil {
			return fmt.Errorf("anthropic: text block carries members of another arm")
		}
	case anthropicToolUse:
		if raw.ID == "" || raw.Name == "" {
			return fmt.Errorf("anthropic: tool_use block requires id and name")
		}
		if raw.Text != "" || raw.Thinking != "" || raw.Data != "" || raw.ToolUseID != "" || raw.Content != nil {
			return fmt.Errorf("anthropic: tool_use block carries members of another arm")
		}
	case anthropicToolResult:
		if raw.ToolUseID == "" {
			return fmt.Errorf("anthropic: tool_result block requires tool_use_id")
		}
		if raw.Text != "" || raw.ID != "" || raw.Name != "" || len(raw.Input) > 0 || raw.Thinking != "" || raw.Data != "" {
			return fmt.Errorf("anthropic: tool_result block carries members of another arm")
		}
	case anthropicThinking:
		if raw.Text != "" || raw.ID != "" || raw.Name != "" || len(raw.Input) > 0 || raw.ToolUseID != "" || raw.Data != "" || raw.Content != nil {
			return fmt.Errorf("anthropic: thinking block carries members of another arm")
		}
	case anthropicRedacted:
		if raw.Text != "" || raw.Thinking != "" || raw.ID != "" || raw.Name != "" || len(raw.Input) > 0 || raw.ToolUseID != "" || raw.Content != nil {
			return fmt.Errorf("anthropic: redacted_thinking block carries members of another arm")
		}
	}
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
	// Accepted-domain closure: max_tokens must be in 1..MaxInt32 (the
	// canonical field the SDK pins); anything else is refused here as a
	// 400-class parse error, never silently clamped or re-emitted.
	if ar.MaxTokens != nil && (*ar.MaxTokens < 1 || *ar.MaxTokens > math.MaxInt32) {
		return nil, fmt.Errorf("anthropic: max_tokens %d is outside 1..%d", *ar.MaxTokens, math.MaxInt32)
	}

	chat := &engine.ChatRequest{
		Model:         ar.Model,
		Stream:        ar.Stream,
		MaxTokens:     ar.MaxTokens,
		Temperature:   ar.Temperature,
		TopP:          ar.TopP,
		StopSequences: ar.StopSequences,
	}

	// Provider extensions: parse the ORIGINAL body and delete the canonical
	// fields in fixed order — deterministic, lexeme-exact unknown members.
	ext, xerr := engine.ParseOptionalJSONObject(rawBody)
	if xerr != nil {
		return nil, fmt.Errorf("provider extensions: %w", xerr)
	}
	ext, xerr = ext.WithoutMembers("model", "max_tokens", "temperature", "top_p",
		"stop_sequences", "system", "messages", "tools", "stream")
	if xerr != nil {
		return nil, fmt.Errorf("provider extensions: %w", xerr)
	}
	if ext, xerr = format.NormalizeExtensionObject(ext); xerr != nil {
		return nil, fmt.Errorf("provider extensions: %w", xerr)
	}
	if !ext.IsAbsent() {
		chat.ProviderExtensions = ext
	}

	// System: text blocks in wire order with their positional cache
	// breakpoints. The strict TextBlockParam parsing (parse-bypass) stays:
	// every system element must be a text block, so only text + cache
	// breakpoints are representable here.
	if len(ar.System) > 0 {
		var blocks []engine.Block
		for _, b := range ar.System {
			// Empty text is absent-like for the system array: `"system": ""`
			// and `{"type":"text","text":""}` produce no system message
			// (explicit-empty is a first-class arm for MESSAGE bodies, not
			// for the system preamble).
			if b.Text == "" {
				continue
			}
			blocks = append(blocks, engine.Block{Text: &engine.TextBlock{Text: b.Text}})
			if b.CacheControl != nil {
				marker, err := engine.ParseRequiredJSONObject(mustMarshalA(b.CacheControl))
				if err != nil {
					return nil, fmt.Errorf("system cache_control: %w", err)
				}
				blocks = append(blocks, engine.Block{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: marker}})
			}
		}
		// An all-empty system array (e.g. `"system": ""`) is absent-like:
		// no message, exactly as if the member were missing.
		if len(blocks) > 0 {
			chat.Messages = append(chat.Messages, engine.Message{Role: engine.RoleSystem, Blocks: blocks})
		}
	}

	// Messages: project the wire content array onto the ordered block
	// sequence ONE message at a time — provider message boundaries and roles
	// stay intact (tool results ride their user message at their exact
	// position; there is no synthetic RoleTool split).
	for _, am := range ar.Messages {
		msg := engine.Message{Role: mapRole(am.Role)}
		for i, block := range am.Content {
			blk, cerr := anthropicBlockToEngine(block, i, am.ContentRaw)
			if cerr != nil {
				return nil, cerr
			}
			msg.Blocks = append(msg.Blocks, blk)
			// A cache_control member on a content block closes the cached
			// prefix at that position: project it as a CacheBreakpoint block
			// AFTER the covered block (the canonical positional form). This
			// includes tool_result and image blocks — the member is a
			// block-level fact, never folded into the payload (the
			// projection invariant).
			if block.CacheControl != nil {
				marker, merr := engine.ParseRequiredJSONObject(mustMarshalA(block.CacheControl))
				if merr != nil {
					return nil, fmt.Errorf("content cache_control: %w", merr)
				}
				msg.Blocks = append(msg.Blocks, engine.Block{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: marker}})
			}
		}
		chat.Messages = append(chat.Messages, msg)
	}

	// Tools.
	for _, t := range ar.Tools {
		params, err := engine.ParseRequiredObjectOrEmpty(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool %q input_schema: %w", t.Name, err)
		}
		td := engine.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		}
		if t.CacheControl != nil {
			cc, cerr := engine.ParseRequiredJSONObject(mustMarshalA(t.CacheControl))
			if cerr != nil {
				return nil, fmt.Errorf("tool %q cache_control: %w", t.Name, cerr)
			}
			td.CacheControl, _ = engine.ParseOptionalJSONObject(cc.Bytes())
		}
		chat.Tools = append(chat.Tools, td)
	}

	return chat, nil
}

// anthropicBlockKinds are the wire block kinds the adapter models; anything
// else is preserved as an Unknown block (kind + payload with the
// discriminant and cache member removed).
const (
	anthropicText       = "text"
	anthropicThinking   = "thinking"
	anthropicRedacted   = "redacted_thinking"
	anthropicToolUse    = "tool_use"
	anthropicToolResult = "tool_result"
	anthropicImage      = "image"
)

// anthropicBlockToEngine projects one wire content block onto the ordered
// body. Tool-result content arrays become nested tool-result content; image
// and unknown arms become Unknown blocks with the discriminant ("type") and
// any cache member removed from the payload.
func anthropicBlockToEngine(block contentBlock, i int, contentRaw json.RawMessage) (engine.Block, error) {
	switch block.Type {
	case anthropicText:
		return engine.Block{Text: &engine.TextBlock{Text: block.Text}}, nil
	case anthropicThinking:
		return engine.Block{Thinking: &engine.ThinkingBlock{Text: block.Thinking, Signature: block.Signature}}, nil
	case anthropicRedacted:
		return engine.Block{RedactedThinking: &engine.RedactedThinkingBlock{Data: block.Data}}, nil
	case anthropicToolUse:
		args, err := engine.ParseRequiredObjectOrEmpty(block.Input)
		if err != nil {
			return engine.Block{}, fmt.Errorf("tool_use %q input: %w", block.Name, err)
		}
		return engine.Block{ToolUse: &engine.ToolUseBlock{
			ID: block.ID, Name: block.Name, Arguments: args,
		}}, nil
	case anthropicToolResult:
		tr := &engine.ToolResultBlock{ToolCallID: block.ToolUseID, ToolName: block.Name}
		if s, ok := block.Content.(string); ok {
			tr.Content = []engine.ToolResultContentBlock{{Text: s}}
			return engine.Block{ToolResult: tr}, nil
		}
		if arr, ok := block.Content.([]any); ok {
			// Raw nested elements, aligned by index with arr: decode the
			// block's raw "content" member so lexemes survive the
			// projection (a map re-encode would lose them).
			var rawNested []json.RawMessage
			if blockRaw := rawElement(contentRaw, i); blockRaw != nil {
				var rawTR struct {
					Content json.RawMessage `json:"content"`
				}
				if json.Unmarshal(blockRaw, &rawTR) == nil {
					json.Unmarshal(rawTR.Content, &rawNested)
				}
			}
			for j, p := range arr {
				elem, err := anthropicNestedContentToEngine(p, rawNested, j)
				if err != nil {
					return engine.Block{}, err
				}
				tr.Content = append(tr.Content, elem)
			}
			if len(tr.Content) == 0 {
				tr.Content = []engine.ToolResultContentBlock{{Text: ""}}
			}
			return engine.Block{ToolResult: tr}, nil
		}
		return engine.Block{}, fmt.Errorf("tool_result content: expected string or array")
	default:
		// image and any unmodelled arm: preserve the raw element with the
		// discriminant and cache member removed (the projection invariant;
		// the kind is the single authority).
		kind := block.Type
		if kind == "" {
			kind = "unknown"
		}
		raw := rawElement(contentRaw, i)
		payload, err := stripBlockFacts(raw, "type", "cache_control")
		if err != nil {
			return engine.Block{}, fmt.Errorf("block %q payload: %w", kind, err)
		}
		return engine.Block{Unknown: &engine.UnknownBlock{Kind: kind, Payload: payload}}, nil
	}
}

// anthropicNestedContentToEngine projects one tool-result content element
// onto the nested kinds (text / unknown / cache breakpoint). rawNested holds
// the block's raw content array, aligned by index with the typed elements.
func anthropicNestedContentToEngine(p any, rawNested []json.RawMessage, j int) (engine.ToolResultContentBlock, error) {
	if m, ok := p.(map[string]any); ok {
		if t, _ := m["type"].(string); t == anthropicText || t == "" {
			if txt, ok := m["text"].(string); ok {
				return engine.ToolResultContentBlock{Text: txt}, nil
			}
		}
		if cc, ok := m["cache_control"].(map[string]any); ok && cc != nil {
			marker, err := engine.ParseRequiredJSONObject(mustMarshalA(cc))
			if err != nil {
				return engine.ToolResultContentBlock{}, fmt.Errorf("nested cache_control: %w", err)
			}
			return engine.ToolResultContentBlock{CacheBreakpoint: &engine.CacheBreakpointBlock{Marker: marker}}, nil
		}
	}
	kind := "unknown"
	if m, ok := p.(map[string]any); ok {
		if t, ok := m["type"].(string); ok && t != "" {
			kind = t
		}
	}
	raw := json.RawMessage(nil)
	if j < len(rawNested) {
		raw = rawNested[j]
	}
	payload, err := stripBlockFacts(raw, "type", "cache_control")
	if err != nil {
		return engine.ToolResultContentBlock{}, fmt.Errorf("nested block %q payload: %w", kind, err)
	}
	return engine.ToolResultContentBlock{Unknown: &engine.UnknownBlock{Kind: kind, Payload: payload}}, nil
}

// rawElement returns element i of a raw JSON array, or nil.
func rawElement(raw json.RawMessage, i int) json.RawMessage {
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var els []json.RawMessage
	if json.Unmarshal(raw, &els) != nil || i < 0 || i >= len(els) {
		return nil
	}
	return els[i]
}

// stripBlockFacts removes the canonical discriminant and cache members from
// a raw block object (span-preserving), so the payload never duplicates the
// block kinds' authority. A missing raw element is a refusal.
func stripBlockFacts(raw json.RawMessage, keys ...string) (engine.RequiredJSONObject, error) {
	if len(raw) > 0 && raw[0] == '{' {
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
	return engine.RequiredJSONObject{}, fmt.Errorf("expected a JSON object block")
}

// mustMarshalA encodes a wire-decoded map back to bytes (used only for
// cache markers, which are small and authored by the provider).
func mustMarshalA(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Marshal converts a canonical ChatRequest into Anthropic Messages JSON.
func (a *Adapter) Marshal(chat *engine.ChatRequest) ([]byte, error) {
	// The owning validation at EVERY marshal entry: the engine pointer sum
	// must be in the closed domain before any arm is projected — a future
	// call site cannot bypass the checked boundary by accident.
	if err := pbconv.ValidateEngineRequest(chat); err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
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

	// System: the first system message's blocks project onto the system
	// array (text blocks; positional cache breakpoints become members on the
	// block they close). Unrepresentable blocks in a system message fail
	// closed with an actionable error.
	for _, m := range chat.Messages {
		if m.Role == engine.RoleSystem {
			sys, err := marshalSystemBlocks(m)
			if err != nil {
				return nil, err
			}
			ar.System = sys
			break // only first system message
		}
	}

	// Messages: the ordered body projects onto the wire content array one
	// message at a time — provider message boundaries and roles stay intact.
	for _, m := range chat.Messages {
		if m.Role == engine.RoleSystem {
			continue // handled above
		}
		if m.Role == engine.RoleTool {
			// Anthropic has no native tool-role message: a tool-role message
			// is unrepresentable on this wire and must fail closed rather
			// than be dropped or reordered.
			return nil, fmt.Errorf("anthropic: tool-role messages are not representable; " +
				"tool results must ride their user message as ToolResult blocks")
		}
		am := anthropicMessage{Role: unmapRole(m.Role)}
		blocks, err := marshalContentBlocks(m)
		if err != nil {
			return nil, err
		}
		am.Content = blocks
		ar.Messages = append(ar.Messages, am)
	}

	// Tools.
	for _, t := range chat.Tools {
		td := anthropicToolDef{
			Name:         t.Name,
			Description:  t.Description,
			InputSchema:  t.Parameters.Bytes(),
			CacheControl: decodeMarker(t.CacheControl),
		}
		if len(td.CacheControl) == 0 {
			td.CacheControl = nil
		}
		ar.Tools = append(ar.Tools, td)
	}

	b, err := json.Marshal(ar)
	if err != nil {
		return nil, err
	}

	if !chat.ProviderExtensions.IsAbsent() {
		var outMap map[string]json.RawMessage
		if err := json.Unmarshal(b, &outMap); err != nil {
			return nil, err
		}
		if err := format.MergeRawMembers(outMap, chat.ProviderExtensions.Bytes()); err != nil {
			return nil, fmt.Errorf("provider extensions merge: %w", err)
		}
		return json.Marshal(outMap)
	}

	return b, nil
}

// marshalSystemBlocks projects a system message's ordered body onto the
// system array. Text blocks emit text blocks; a CacheBreakpoint closes the
// prefix at its position, so it becomes a cache_control MEMBER on the
// preceding text block. Any other block kind, or a breakpoint at position 0,
// is unrepresentable and fails closed.
func marshalSystemBlocks(m engine.Message) ([]contentBlock, error) {
	var out []contentBlock
	for i, b := range m.Blocks {
		switch {
		case b.Text != nil:
			out = append(out, contentBlock{Type: "text", Text: b.Text.Text})
		case b.CacheBreakpoint != nil:
			if i == 0 || len(out) == 0 {
				return nil, fmt.Errorf("anthropic: system cache breakpoint at position %d "+
					"has no preceding text block to attach to", i)
			}
			if out[len(out)-1].CacheControl == nil {
				out[len(out)-1].CacheControl = decodeMarker(b.CacheBreakpoint.Marker)
			}
		default:
			return nil, fmt.Errorf("anthropic: system block %d (%s) is not representable "+
				"on the Anthropic system array", i, blockKind(b))
		}
	}
	return out, nil
}

// marshalContentBlocks projects a message's ordered body onto the wire
// content array, enforcing the Anthropic provider grammar fail-closed:
//
//   - text / thinking / redacted_thinking / tool_use / tool_result map to
//     their wire blocks;
//   - a CacheBreakpoint becomes a cache_control MEMBER on the block it
//     closes (breakpoint at position 0 is unrepresentable);
//   - Unknown blocks re-attach their discriminant as the wire "type";
//   - a trailing signature is unrepresentable (Code Assist only);
//   - tool_use is assistant-only and tool_result is user-only.
func marshalContentBlocks(m engine.Message) ([]contentBlock, error) {
	var out []contentBlock
	for i, b := range m.Blocks {
		var cb contentBlock
		switch {
		case b.Text != nil:
			cb = contentBlock{Type: "text", Text: b.Text.Text}
		case b.Thinking != nil:
			cb = contentBlock{Type: "thinking", Thinking: b.Thinking.Text, Signature: b.Thinking.Signature}
		case b.RedactedThinking != nil:
			cb = contentBlock{Type: "redacted_thinking", Data: b.RedactedThinking.Data}
		case b.ToolUse != nil:
			if m.Role != engine.RoleAssistant {
				return nil, fmt.Errorf("anthropic: tool_use block %d on a %q message is unrepresentable",
					i, m.Role)
			}
			cb = contentBlock{
				Type:  "tool_use",
				ID:    b.ToolUse.ID,
				Name:  b.ToolUse.Name,
				Input: b.ToolUse.Arguments.Bytes(),
			}
		case b.ToolResult != nil:
			if m.Role != engine.RoleUser {
				return nil, fmt.Errorf("anthropic: tool_result block %d on a %q message is unrepresentable",
					i, m.Role)
			}
			cb = contentBlock{
				Type:      "tool_result",
				ToolUseID: b.ToolResult.ToolCallID,
				Name:      b.ToolResult.ToolName,
			}
			content, cerr := marshalNestedContent(b.ToolResult.Content)
			if cerr != nil {
				return nil, cerr
			}
			cb.Content = content
		case b.CacheBreakpoint != nil:
			if i == 0 || len(out) == 0 {
				return nil, fmt.Errorf("anthropic: cache breakpoint at position %d "+
					"has no preceding content block to attach to", i)
			}
			if out[len(out)-1].CacheControl == nil {
				out[len(out)-1].CacheControl = decodeMarker(b.CacheBreakpoint.Marker)
			}
			continue
		case b.Unknown != nil:
			var uerr error
			cb, uerr = marshalUnknownBlock(b.Unknown)
			if uerr != nil {
				return nil, fmt.Errorf("anthropic: %w", uerr)
			}
		case b.TrailingSignature != nil:
			return nil, fmt.Errorf("anthropic: trailing signature block %d is not representable "+
				"(Code Assist only)", i)
		default:
			return nil, fmt.Errorf("anthropic: block %d has no arm", i)
		}
		out = append(out, cb)
	}
	return out, nil
}

// marshalNestedContent projects nested tool-result content onto the wire
// array (text blocks and unknown arms with their discriminants re-attached).
// Nested cache breakpoints become cache_control members on the preceding
// nested element (position 0 is unrepresentable).
func marshalNestedContent(content []engine.ToolResultContentBlock) (any, error) {
	if len(content) == 0 {
		return "", nil
	}
	var out []any
	for i, c := range content {
		switch {
		case c.Text != "" || (c.Unknown == nil && c.CacheBreakpoint == nil):
			out = append(out, map[string]any{"type": "text", "text": c.Text})
		case c.Unknown != nil:
			payload, _, err := c.Unknown.Payload.DecodeObject()
			if err != nil {
				return nil, fmt.Errorf("nested unknown payload: %w", err)
			}
			for _, canon := range []string{"type", "cache_control"} {
				if _, dup := payload[canon]; dup {
					return nil, fmt.Errorf("anthropic: nested unknown payload duplicates canonical member %q (projection invariant)", canon)
				}
			}
			if c.Unknown.Kind == anthropicText || c.Unknown.Kind == anthropicToolResult {
				return nil, fmt.Errorf("anthropic: nested unknown block kind %q names a modeled arm (projection invariant)", c.Unknown.Kind)
			}
			block := make(map[string]any, len(payload)+1)
			block["type"] = c.Unknown.Kind
			for k, v := range payload {
				block[k] = json.RawMessage(v)
			}
			out = append(out, block)
		case c.CacheBreakpoint != nil:
			if i == 0 || len(out) == 0 {
				return nil, fmt.Errorf("anthropic: nested cache breakpoint at position %d "+
					"has no preceding element to attach to", i)
			}
			prev, ok := out[len(out)-1].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("anthropic: nested cache breakpoint after a non-object element")
			}
			if _, exists := prev["cache_control"]; !exists {
				prev["cache_control"] = decodeMarker(c.CacheBreakpoint.Marker)
			}
		}
	}
	if len(out) == 0 {
		return "", nil
	}
	return out, nil
}

// marshalUnknownBlock re-attaches the discriminant as the wire "type".
func marshalUnknownBlock(u *engine.UnknownBlock) (contentBlock, error) {
	payload, _, err := u.Payload.DecodeObject()
	if err != nil {
		return contentBlock{}, fmt.Errorf("unknown payload: %w", err)
	}
	// The projection invariant is executable here: the payload must never
	// duplicate the canonical discriminant or the cache member — a
	// plugin-provided payload that does would silently override the kind or
	// smuggle a cache fact the verifier never saw. A kind that names a
	// MODELED arm would fabricate a block the verifier never saw. Reject
	// before marshal.
	for _, canon := range []string{"type", "cache_control"} {
		if _, dup := payload[canon]; dup {
			return contentBlock{}, fmt.Errorf("anthropic: unknown payload duplicates canonical member %q (projection invariant)", canon)
		}
	}
	switch u.Kind {
	case anthropicText, anthropicThinking, anthropicRedacted, anthropicToolUse, anthropicToolResult:
		return contentBlock{}, fmt.Errorf("anthropic: unknown block kind %q names a modeled arm (projection invariant)", u.Kind)
	}
	block := make(map[string]any, len(payload)+1)
	block["type"] = u.Kind
	for k, v := range payload {
		block[k] = json.RawMessage(v)
	}
	b, err := json.Marshal(block)
	if err != nil {
		return contentBlock{}, err
	}
	var cb contentBlock
	if err := json.Unmarshal(b, &cb); err != nil {
		return contentBlock{}, err
	}
	return cb, nil
}

// decodeMarker decodes a marker object for the wire map shape. Accepts the
// required wrapper (block markers) and the optional wrapper (tool-def
// markers); absent/zero yields nil.
func decodeMarker(m interface {
	Bytes() []byte
}) map[string]any {
	var out map[string]any
	if err := json.Unmarshal(m.Bytes(), &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// blockKind names a block for error messages.
func blockKind(b engine.Block) string {
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
