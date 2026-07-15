package engine

// --- Request side ---

// ChatRequest is the canonical representation of a chat completion request
// regardless of provider wire format.
type ChatRequest struct {
	Model              string // model name as sent by client (e.g. "deepseek-v4-pro")
	Messages           []Message
	Tools              []ToolDef
	Stream             bool
	MaxTokens          *int
	Temperature        *float64
	TopP               *float64
	StopSequences      []string
	SafetySettings     []any          // Google Vertex/Gemini safety configuration
	ProviderExtensions map[string]any // unparsed fields passed through transparently

	// ToranaMeta carries proxy-internal metadata that format adapters
	// MUST NOT serialize to the wire. Used for request-scoped state
	// (e.g. mutation registries) shared between hooks.
	ToranaMeta map[string]any
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
type Message struct {
	Role              Role
	Content           string     // text body; empty for tool-call-only messages
	ContentParts      []any      // multimodal array content (e.g. vision)
	Thinking          string     // extended thinking / reasoning text
	ThinkingSignature string     // Anthropic cryptographic signature (empty for other providers)
	RedactedThinking  string     // encrypted/redacted thinking blocks from Anthropic
	ToolCalls         []ToolCall // assistant → tool invocations
	ToolCallID        string     // tool messages: which call this result answers
	ToolName          string     // tool messages: which tool produced this result
}

// ToolCall represents an assistant's request to invoke a tool.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any // parsed JSON object
}

// ToolDef describes a function available to the model.
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema object: {"type":"object","properties":{...},"required":[...]}
	Strict      bool
}

// --- Response streaming side ---

// StreamEvent is a single event emitted during a streaming response.
// Exactly one field is non-nil per event. Consumers switch on the non-nil field.
type StreamEvent struct {
	// Exactly one field is non-nil per event.
	TextDelta     *string        // text content fragment
	ThinkingDelta *string        // thinking/reasoning text fragment
	ToolCallStart *ToolCallStart // new tool call beginning
	ToolCallDelta *ToolCallDelta // arguments JSON fragment (string)
	ToolCallEnd   *ToolCallEnd   // tool call arguments complete
	FinishReason  string         // "stop", "tool_calls", "length", "error"
	Usage         *StreamUsage   // token usage from stream (OpenAI final chunk, Anthropic usage event)
	Error         *StreamError
}

// ToolCallStart signals the beginning of a tool call in the stream.
type ToolCallStart struct {
	Index int // 0-based within this turn (OpenAI uses index for parallel calls)
	ID    string
	Name  string
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
type StreamUsage struct {
	InputTokens  int
	OutputTokens int
}
