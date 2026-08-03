package engine

type chatRequestCtxKey struct{}

// ChatRequestKey is the context key used to store/retrieve *ChatRequest.
var ChatRequestKey = chatRequestCtxKey{}

// --- Request side ---

// ChatRequest is the canonical representation of a chat completion request
// regardless of provider wire format.
type ChatRequest struct {
	Model         string // model name as sent by client (e.g. "deepseek-v4-pro")
	Messages      []Message
	Tools         []ToolDef
	Stream        bool
	MaxTokens     *int
	Temperature   *float64
	TopP          *float64
	StopSequences []string
	// SafetySettings is the authoritative raw safety-config array (Gemini
	// shape); absent vs present-empty are distinct.
	SafetySettings OptionalJSONArray
	// ProviderExtensions carries unknown top-level provider fields as the
	// authoritative raw object (deterministic member order, lexeme-exact).
	ProviderExtensions OptionalJSONObject
	// ToranaMeta carries proxy-internal metadata that format adapters MUST
	// NOT serialize to the wire. Used for request-scoped state (e.g.
	// mutation registries) shared between hooks. Host-owned.
	ToranaMeta OptionalJSONObject
}

// Role classifies a message's speaker.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message represents a single turn in a chat conversation.
// For simple text messages, Content holds the text body and tool fields are zero.
// For assistant tool-call messages, Content is empty and ToolCalls is populated.
// For tool-result messages, ToolCallID identifies the call and ToolName names the tool.
// Message represents a single turn in a chat conversation.
//
// The message BODY is the ordered Block sequence — the SOLE authority for
// every content fact (text, thinking, tool use, tool results, cache
// breakpoints, unknown provider arms, signatures), in provider wire order.
// See block.go for the block kinds and their absolute rules.
type Message struct {
	Role   Role
	Blocks []Block
}

// ToolDef describes a function available to the model.
type ToolDef struct {
	Name        string
	Description string
	// Parameters is the REQUIRED authoritative JSON Schema object (the
	// RequiredJSONObject wrapper): zero = canonical `{}` (a valid
	// unconstrained schema), never absent, validated raw lexemes.
	Parameters RequiredJSONObject
	Strict     bool
	// CacheControl marks a cache breakpoint after this tool definition
	// (Anthropic allows cache_control on tool entries). Opaque raw marker
	// object; absent when zero.
	CacheControl OptionalJSONObject
}

// --- Response streaming side ---

// StreamEvent is a single event emitted during a streaming response.
// Exactly one field is non-nil per event (SignatureDelta is metadata paired
// with the surrounding block rather than a content event; see below).
// Consumers switch on the non-nil field.
type StreamEvent struct {
	// Exactly one field is non-nil per event.
	TextDelta     *string        // text content fragment
	ThinkingDelta *string        // thinking/reasoning text fragment
	BlockStart    *BlockStart    // opens an explicit text/thinking/provider content block
	BlockStop     *BlockStop     // closes the text/thinking/provider block opened at the same index
	ToolCallStart *ToolCallStart // new tool call beginning
	ToolCallDelta *ToolCallDelta // arguments JSON fragment (string)
	ToolCallEnd   *ToolCallEnd   // tool call arguments complete
	FinishReason  string         // "stop", "tool_calls", "length", "error"
	Usage         *StreamUsage   // token usage from stream (OpenAI final chunk, Anthropic usage event)
	Error         *StreamError

	// SignatureDelta carries an opaque provider signature (e.g. Gemini
	// thoughtSignature on a text/thought part) that must be preserved on
	// re-serialization. It is metadata paired with the surrounding block, not
	// an "exactly one field" content event; adapters that don't understand it
	// ignore it.
	SignatureDelta *string
}

// BlockKind classifies an explicit non-tool content block.
type BlockKind int

const (
	BlockKindText BlockKind = iota
	BlockKindThinking
	BlockKindProvider
)

func (k BlockKind) String() string {
	switch k {
	case BlockKindText:
		return "text"
	case BlockKindThinking:
		return "thinking"
	case BlockKindProvider:
		return "provider"
	default:
		return "unknown"
	}
}

// BlockStart opens an explicit text/thinking/provider content block at Index.
// Tool-call blocks keep ToolCallStart, which already carries the block payload
// (id/name/signature). The v2 ABI requires every content block to open with a
// start event so signatures can be bound to the right scope.
type BlockStart struct {
	Index int
	Kind  BlockKind
	// ProviderKind is the provider's verbatim kind string for a provider
	// block (the v2 ABI passes ProviderBlock.kind through verbatim; the host
	// must not change it after verification). Meaningful only when Kind ==
	// BlockKindProvider; empty for text/thinking blocks.
	ProviderKind string
}

// BlockStop closes the text/thinking/provider block opened at Index.
// Tool-call blocks keep ToolCallEnd.
type BlockStop struct {
	Index int
}

// ToolCallStart signals the beginning of a tool call in the stream.
type ToolCallStart struct {
	Index     int // 0-based within this turn (OpenAI uses index for parallel calls)
	ID        string
	Name      string
	Signature string // opaque provider token on the call (Gemini thoughtSignature); empty otherwise
}

// ToolCallDelta carries a fragment of tool call arguments JSON.
type ToolCallDelta struct {
	Index          int
	ArgumentsDelta string // raw JSON fragment; concatenate + parse at end
}

// ToolCallEnd signals that a tool call's arguments are complete.
type ToolCallEnd struct {
	Index int
}

// StreamError represents a streaming error event.
type StreamError struct {
	Code    int
	Message string
}

// StreamUsage represents token usage data from a streaming response.
// InputTokens excludes cached tokens for providers that report them
// separately (Anthropic); for providers where the cached count is a subset
// of the prompt total (OpenAI, Gemini, Bedrock) it is the full prompt count.
type StreamUsage struct {
	InputTokens  int
	OutputTokens int
	// CacheReadTokens is the number of input tokens served from the
	// provider's prompt cache (billed at a fraction of full price).
	CacheReadTokens int
	// CacheWriteTokens is the number of input tokens written to the cache
	// this turn (Anthropic cache_creation_input_tokens, Bedrock
	// cacheWriteInputTokens); 0 for providers that don't report it.
	CacheWriteTokens int
}

// --- Response side ---

// ResponseMessage is the assistant's reply in a completed response, in the
// narrow shape the host can actually apply: presence-preserving content and
// in-place tool-call mutations only. It deliberately is not Message — request
// semantics (role, thinking, content parts, cache control) have no writable
// response counterpart, and reusing Message would claim a writable surface
// the host does not deliver.
//
// The relative constraints (content presence identical to the accepted
// response, fixed tool-call cardinality and positional order) are enforced by
// the host pipeline per plugin before any replacement is accepted, and
// re-verified at the apply boundary.
type ResponseMessage struct {
	// Content is the assistant's text with proto presence preserved: nil
	// means the provider body has no writable text slot, a non-nil pointer
	// (possibly to "") means a present text part. Present-empty is not
	// absent, and a plugin cannot change presence — only the value.
	Content *string
	// ToolCalls carries the response's tool invocations in provider order.
	// Fixed cardinality: a plugin may mutate element N in place but cannot
	// add, remove, or reorder calls. ID and Signature are host-owned.
	ToolCalls []ResponseToolCall
}

// ResponseToolCall is one tool invocation in a completed response.
//
// ArgumentsJSON is the provider's verbatim arguments bytes. It must never be
// decoded and re-encoded on the canonical path: key order is part of the
// cacheable prompt prefix, and an integer above JavaScript's exact range
// cannot survive a float64 round-trip.
type ResponseToolCall struct {
	ID   string
	Name string
	// ArgumentsJSON is the provider's raw arguments object, byte for byte.
	ArgumentsJSON []byte
	// Signature is an opaque provider-specific token attached to this call
	// (e.g. Gemini/Code Assist thoughtSignature). Preserved across a
	// round-trip; the only permitted transition is the host clearing an
	// existing token whose covered content changed. A plugin cannot mint one.
	Signature string
}

// ChatResponse is the canonical representation of a completed chat response,
// regardless of provider wire format.
//
// It exists because v1 had no response type, so run_after_response was handed a
// ChatRequest and three different call paths filled it three incompatible ways:
// a synthesized assistant message on the non-streaming path, model + metadata
// with no messages on the upstream-error path, and — on the streaming path —
// the outbound REQUEST, whose Messages are the conversation history rather than
// the reply. A plugin reading the assistant's answer got the last user message
// instead, depending on a path it could not observe.
//
// Mirrors torana.v2.ChatResponse field for field so the conversion cannot lose
// or invent anything.
type ChatResponse struct {
	Model string
	ID    string
	// Message is the assistant's reply: exactly one message, because a
	// response IS one message. The v1 shape allowed a list and thereby allowed
	// the ambiguity above.
	//
	// ResponseMessage, not Message: the response side has its own narrow shape
	// (presence-preserving content, raw arguments bytes) and reusing the
	// request Message would let the two drift.
	Message *ResponseMessage
	// FinishReason is the provider's stop reason ("stop", "tool_use", ...).
	FinishReason string
	Usage        *StreamUsage
	// UpstreamStatus is the provider's HTTP status. Non-2xx means Message is
	// usually absent, which is why plugins must not assume it is set.
	UpstreamStatus int
	// DurationMS is how long the upstream call took.
	DurationMS int64
	// ProviderExtensions carries unparsed provider fields passed through
	// transparently, as on the request side.
	ProviderExtensions map[string]any
}
