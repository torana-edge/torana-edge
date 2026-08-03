package engine

// Ordered message body (the SDK ordered-body ABI, Edge side).
//
// A Message's content is the ordered Block sequence — the SOLE authority for
// every content fact, in provider wire order. There are no competing flat
// fields. The block kinds mirror pb/v2 RequestBlock one-for-one:
//
//	Text              — plain text (explicit empty is a first-class arm)
//	Thinking          — extended reasoning, with the current-block token
//	RedactedThinking  — provider redaction placeholder
//	ToolUse           — assistant tool invocation (identity + arguments)
//	ToolResult        — tool result at its wire position (ordered nested content)
//	CacheBreakpoint   — positional cache breakpoint (closes the cached prefix)
//	Unknown           — provider arm Torana does not model (kind + raw payload)
//	TrailingSignature — standalone final token (Code Assist)
//
// Rules (absolute; enforced by the SDK validator and mirrored by FromPB's
// totality):
//   - a message carries at least one block;
//   - a trailing signature requires at least one preceding text/thinking
//     block, is assistant-only, and is final;
//   - tool-result nested content is non-empty (one explicit empty text
//     element is the canonical empty-result spelling);
//   - nil blocks and typed-nil arms are refusals, never panics.

// Block is one ordered element of a message body. Exactly one field is
// non-nil per block.
type Block struct {
	Text              *TextBlock
	Thinking          *ThinkingBlock
	RedactedThinking  *RedactedThinkingBlock
	ToolUse           *ToolUseBlock
	ToolResult        *ToolResultBlock
	CacheBreakpoint   *CacheBreakpointBlock
	Unknown           *UnknownBlock
	TrailingSignature *TrailingSignatureBlock
}

// TextBlock is plain text. Explicit empty text is a first-class arm
// (provider-visible position must survive).
type TextBlock struct {
	Text string
	// Signature is the content-bound provenance token (Gemini/Code Assist
	// thoughtSignature beside non-thought text). Empty when the provider
	// sent none; host-side provenance rules govern it.
	Signature string
	// PartMetadataJson is the provider Part-level custom metadata
	// (Gemini partMetadata), absent or a strict JSON object. Covered by
	// the block's signature binding.
	PartMetadataJson OptionalJSONObject
}

// ThinkingBlock is extended thinking / reasoning text.
type ThinkingBlock struct {
	Text      string
	Signature string // current-block provenance token
	// PartMetadataJson is provider Part-level custom metadata, absent or a
	// strict JSON object; covered by this block's signature binding.
	PartMetadataJson OptionalJSONObject
}

// RedactedThinkingBlock is a provider redaction placeholder.
type RedactedThinkingBlock struct {
	Data string
}

// ToolUseBlock is an assistant's request to invoke a tool.
type ToolUseBlock struct {
	ID   string
	Name string
	// Arguments is the REQUIRED authoritative JSON object (zero = canonical
	// `{}`), raw validated lexemes.
	Arguments RequiredJSONObject
	// Signature is the call-bound provenance token (e.g. Gemini
	// thoughtSignature). Host-side provenance rules govern it.
	Signature string
	// PartMetadataJson is provider Part-level custom metadata, absent or a
	// strict JSON object; covered by this block's signature binding.
	PartMetadataJson OptionalJSONObject
}

// ToolResultBlock is a tool result at its exact wire position.
type ToolResultBlock struct {
	ToolCallID string
	ToolName   string
	// Content is the ordered NESTED content of the result. Dedicated kinds
	// only (text/unknown/cache): nested tool use/result/thinking/signature
	// is unrepresentable. Non-empty: one explicit empty text element is the
	// canonical empty-result spelling.
	Content []ToolResultContentBlock
	// PartMetadataJson is provider Part-level custom metadata, absent or a
	// strict JSON object; covered by this block's signature binding.
	PartMetadataJson OptionalJSONObject
	// WillContinue is the gemini functionResponse willContinue: PRESENCE is
	// meaningful (explicit false differs from absent); only applicable to
	// NON_BLOCKING function calls.
	WillContinue *bool
	// Scheduling is the gemini functionResponse scheduling: the exact wire
	// enum string (SILENT/WHEN_IDLE/INTERRUPT) or absent (provider default
	// WHEN_IDLE). Presence is meaningful; the vocabulary is the adapter's
	// boundary rule (unknown values are the value-free 400).
	Scheduling *string
	// Signature is the opaque provider token bound to this result (gemini
	// thoughtSignature on a functionResponse part). Provenance-governed
	// exactly like the other request tokens.
	Signature string
}

// ToolResultContentBlock is one ordered element of a tool result's nested
// content. Exactly one field is non-zero per element.
type ToolResultContentBlock struct {
	Text            string // when the element is text
	Unknown         *UnknownBlock
	CacheBreakpoint *CacheBreakpointBlock
}

// CacheBreakpointBlock is a provider cache breakpoint at an explicit
// position: it CLOSES the cached prefix at its position (after the content
// it covers). Multiple markers per message are naturally representable.
type CacheBreakpointBlock struct {
	// Marker is the REQUIRED opaque marker object (e.g. Anthropic
	// {"type":"ephemeral"}), raw validated lexemes.
	Marker RequiredJSONObject
}

// UnknownBlock is a provider content arm Torana does not model, preserved at
// its exact wire position. The payload MUST NOT carry the canonical
// discriminant or a canonical cache member — the block kinds are the single
// authority for those facts. The SDK proves kind + strict object; the
// kind-specific projection invariant is each provider adapter's marshal
// validator (an executable Edge obligation): a returned payload duplicating
// a canonical fact is rejected before marshal.
type UnknownBlock struct {
	Kind    string
	Payload RequiredJSONObject
	// PartMetadataJson is provider Part-level custom metadata, absent or a
	// strict JSON object; covered by this block's signature binding.
	PartMetadataJson OptionalJSONObject
	// Signature is the opaque provider token bound to this unknown part
	// (gemini thoughtSignature on a media/future arm).
	Signature string
}

// TrailingSignatureBlock is Code Assist's trailing signature-only part: the
// standalone token binding the preceding CLOSED text/thinking content of the
// message (never tool-call blocks). Assistant-only, singular, and FINAL.
type TrailingSignatureBlock struct {
	Signature string
	// PartMetadataJson is provider Part-level custom metadata (the trailing
	// standalone is a real Gemini Part), absent or a strict JSON object.
	PartMetadataJson OptionalJSONObject
}

// TextBlocks returns the text of every text block in wire order.
func (m *Message) TextBlocks() []string {
	if m == nil {
		return nil
	}
	var out []string
	for _, b := range m.Blocks {
		if b.Text != nil {
			out = append(out, b.Text.Text)
		}
	}
	return out
}

// Text returns the concatenation of every text block's text in wire order.
func (m *Message) Text() string {
	var out string
	for _, t := range m.TextBlocks() {
		out += t
	}
	return out
}

// hasCoveredBlock reports whether a text or thinking block exists — the
// trailing-signature token requires a preceding covered block.
func (m *Message) hasCoveredBlock() bool {
	for _, b := range m.Blocks {
		if b.Text != nil || b.Thinking != nil {
			return true
		}
	}
	return false
}
