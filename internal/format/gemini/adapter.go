// Package gemini implements format adapters for the Google Gemini generateContent
// wire format — the content model (contents/parts/functionCall/systemInstruction/
// generationConfig) shared by the public Gemini API, Vertex AI, and the Code
// Assist API behind the Antigravity CLI (agy).
//
// Two endpoint flavors are registered as sibling formats sharing this code:
//
//   - "gemini": the bare shape used by the public Gemini API and Vertex AI —
//     {systemInstruction, contents, tools, …} at the JSON root; SSE frames are
//     bare `data: {<GenerateContentResponse>}`.
//   - "gemini-codeassist": the Antigravity CLI / Code Assist envelope —
//     {model, project, request:{<GenerateContentRequest>}, …} on the request,
//     and `data: {"response":{<GenerateContentResponse>}}` SSE frames. Tool
//     calls AND results live under role "model", each functionCall/Response
//     carries a real "id", and model parts carry a "thoughtSignature".
//
// The request Adapter is shared: it detects the Code Assist envelope on the wire
// (unambiguous — a top-level "request" object) and preserves the wrapper plus
// inner extras (toolConfig, labels, sessionId, thinkingConfig, requestId, …)
// verbatim, rebuilding only contents/systemInstruction/tools from the IR. Only
// the SSE framing differs between the two, so the StreamAdapter is parameterized
// by a Wrapped flag.
package gemini

import (
	"encoding/json"
	"fmt"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
)

// Format names registered by this package.
const (
	FormatGemini     = "gemini"            // public Gemini API / Vertex AI (bare)
	FormatCodeAssist = "gemini-codeassist" // Code Assist / Antigravity CLI (wrapped)
)

func init() {
	// Bare Gemini (public Gemini API, Vertex AI): unwrapped SSE frames.
	format.Register("/gemini", format.Format{
		Name:    FormatGemini,
		Request: &Adapter{},
		Stream:  &StreamAdapter{Wrapped: false},
	})
	// Code Assist (Antigravity CLI): {"response":…}-wrapped SSE frames.
	format.Register("/gemini-codeassist", format.Format{
		Name:    FormatCodeAssist,
		Request: &Adapter{},
		Stream:  &StreamAdapter{Wrapped: true},
	})
}

// Adapter translates between Gemini/Code Assist JSON and canonical ChatRequest.
// It is shared by both formats; the Code Assist envelope is detected on the wire
// and round-tripped via ProviderExtensions, independent of the format name.
type Adapter struct{}

// ProviderExtensions keys used to round-trip the Code Assist envelope.
const (
	extCodeAssist   = "_codeassist"    // bool marker: request arrived Code-Assist-wrapped
	extWrapper      = "_wrapper"       // map: wrapper fields except "request"
	extRequestExtra = "_request_extra" // map: inner request fields except contents/systemInstruction/tools
)

// --- Wire types for unmarshal/marshal ---

type geminiRequest struct {
	SystemInstruction *geminiSystemInstruction `json:"systemInstruction,omitempty"`
	Contents          []geminiContent          `json:"contents"`
	Tools             []geminiTool             `json:"tools,omitempty"`
	GenerationConfig  *geminiGenerationConfig  `json:"generationConfig,omitempty"`
	SafetySettings    []any                    `json:"safetySettings,omitempty"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type geminiSystemInstruction struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart is a polymorphic content part. Code Assist may combine a
// thoughtSignature with a functionCall or text on the same part.
type geminiPart struct {
	Text             string          `json:"text,omitempty"`
	Thought          bool            `json:"thought,omitempty"`
	ThoughtSignature string          `json:"thoughtSignature,omitempty"`
	FunctionCall     *geminiFuncCall `json:"functionCall,omitempty"`
	FunctionResponse *geminiFuncResp `json:"functionResponse,omitempty"`
}

type geminiFuncCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
	ID   string         `json:"id,omitempty"`
}

type geminiFuncResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
	ID       string         `json:"id,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFuncDecl `json:"functionDeclarations"`
}

type geminiFuncDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// --- Unmarshal ---

// Unmarshal parses a Gemini or Code Assist request body into a ChatRequest.
func (a *Adapter) Unmarshal(rawBody []byte) (*engine.ChatRequest, error) {
	// Detect the Code Assist wrapper: a top-level object with a "request" member.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &top); err != nil {
		return nil, fmt.Errorf("gemini: unmarshal request: %w", err)
	}

	reqBytes := rawBody
	wrapped := false
	if inner, ok := top["request"]; ok && len(inner) > 0 && inner[0] == '{' {
		reqBytes = inner
		wrapped = true
	}

	var gReq geminiRequest
	if err := json.Unmarshal(reqBytes, &gReq); err != nil {
		return nil, fmt.Errorf("gemini: unmarshal inner request: %w", err)
	}

	chat := &engine.ChatRequest{Stream: false, Model: "gemini"}

	if gReq.GenerationConfig != nil {
		chat.MaxTokens = gReq.GenerationConfig.MaxOutputTokens
		chat.Temperature = gReq.GenerationConfig.Temperature
		chat.TopP = gReq.GenerationConfig.TopP
		chat.StopSequences = gReq.GenerationConfig.StopSequences
	}
	if len(gReq.SafetySettings) > 0 {
		chat.SafetySettings = gReq.SafetySettings
	}

	if wrapped {
		chat.ProviderExtensions = codeAssistExtensions(top, reqBytes)
		if m, ok := chat.ProviderExtensions[extWrapper].(map[string]any)["model"].(string); ok && m != "" {
			chat.Model = m
		}
	} else {
		// Bare Gemini: preserve unknown top-level fields as before.
		var raw map[string]any
		if err := json.Unmarshal(rawBody, &raw); err == nil {
			for _, k := range []string{"systemInstruction", "contents", "tools", "generationConfig", "safetySettings"} {
				delete(raw, k)
			}
			if len(raw) > 0 {
				chat.ProviderExtensions = raw
			}
		}
	}

	// System instruction → RoleSystem message.
	if gReq.SystemInstruction != nil {
		var sb string
		for _, p := range gReq.SystemInstruction.Parts {
			if p.Text != "" {
				if sb != "" {
					sb += "\n"
				}
				sb += p.Text
			}
		}
		if sb != "" {
			chat.Messages = append(chat.Messages, engine.Message{Role: engine.RoleSystem, Content: sb})
		}
	}

	// Track synthesized IDs for tool results that lack an explicit id (bare Gemini).
	prevCallIdx := map[string]int{}
	callIDs := map[string]string{}

	for _, content := range gReq.Contents {
		switch content.Role {
		case "user":
			appendUserOrTool(chat, content, callIDs)
		case "model":
			appendModel(chat, content, prevCallIdx, callIDs)
		default:
			// Unknown role: treat text as user text.
			msg := engine.Message{Role: engine.RoleUser}
			for _, p := range content.Parts {
				if p.Text != "" {
					if msg.Content != "" {
						msg.Content += "\n"
					}
					msg.Content += p.Text
				}
			}
			if msg.Content != "" {
				chat.Messages = append(chat.Messages, msg)
			}
		}
	}

	for _, tool := range gReq.Tools {
		for _, decl := range tool.FunctionDeclarations {
			chat.Tools = append(chat.Tools, engine.ToolDef{
				Name:        decl.Name,
				Description: decl.Description,
				Parameters:  decl.Parameters,
			})
		}
	}

	return chat, nil
}

// codeAssistExtensions captures everything needed to reconstruct the wrapper
// and the inner request's non-plugin fields verbatim on Marshal.
func codeAssistExtensions(top map[string]json.RawMessage, reqBytes []byte) map[string]any {
	wrapper := map[string]any{}
	for k, v := range top {
		if k == "request" {
			continue
		}
		var val any
		if json.Unmarshal(v, &val) == nil {
			wrapper[k] = val
		}
	}
	var innerMap map[string]any
	json.Unmarshal(reqBytes, &innerMap)
	// Remove the fields we rebuild from the IR; keep generationConfig,
	// toolConfig, labels, sessionId, safetySettings, … untouched.
	for _, k := range []string{"systemInstruction", "contents", "tools"} {
		delete(innerMap, k)
	}
	return map[string]any{
		extCodeAssist:   true,
		extWrapper:      wrapper,
		extRequestExtra: innerMap,
	}
}

// appendUserOrTool handles a role:"user" content: plain text, or (rarely, for
// bare Gemini) functionResponse parts that become RoleTool messages.
func appendUserOrTool(chat *engine.ChatRequest, content geminiContent, callIDs map[string]string) {
	msg := engine.Message{Role: engine.RoleUser}
	for _, p := range content.Parts {
		switch {
		case p.Text != "":
			if msg.Content != "" {
				msg.Content += "\n"
			}
			msg.Content += p.Text
		case p.FunctionResponse != nil:
			appendToolResult(chat, p.FunctionResponse, callIDs)
		}
	}
	if msg.Content != "" {
		chat.Messages = append(chat.Messages, msg)
	}
}

// appendModel handles a role:"model" content, which under Code Assist may hold
// a functionCall, a functionResponse (tool result), or text — each preserving
// its id and thoughtSignature.
func appendModel(chat *engine.ChatRequest, content geminiContent, prevCallIdx map[string]int, callIDs map[string]string) {
	msg := engine.Message{Role: engine.RoleAssistant}
	for _, p := range content.Parts {
		switch {
		case p.FunctionResponse != nil:
			appendToolResult(chat, p.FunctionResponse, callIDs)
		case p.FunctionCall != nil:
			name := p.FunctionCall.Name
			id := p.FunctionCall.ID
			if id == "" {
				prevCallIdx[name]++
				id = fmt.Sprintf("%s_%d", name, prevCallIdx[name])
			}
			callIDs[name] = id
			msg.ToolCalls = append(msg.ToolCalls, engine.ToolCall{
				ID:        id,
				Name:      name,
				Arguments: p.FunctionCall.Args,
				Signature: p.ThoughtSignature,
			})
		case p.Text != "":
			if msg.Content != "" {
				msg.Content += "\n"
			}
			msg.Content += p.Text
			if p.ThoughtSignature != "" {
				msg.ThinkingSignature = p.ThoughtSignature
			}
		}
	}
	if msg.Content != "" || len(msg.ToolCalls) > 0 {
		chat.Messages = append(chat.Messages, msg)
	}
}

func appendToolResult(chat *engine.ChatRequest, fr *geminiFuncResp, callIDs map[string]string) {
	// Code Assist wraps tool output as {"output": "<text>"}. Store the raw text
	// as Content (real newlines) so compactor plugins can line-split it like any
	// other provider's tool result; re-wrapped on Marshal. Richer responses are
	// kept as JSON.
	content := ""
	if s, ok := singleStringField(fr.Response, "output"); ok {
		content = s
	} else {
		respJSON, err := json.Marshal(fr.Response)
		if err != nil {
			respJSON = []byte("{}")
		}
		content = string(respJSON)
	}
	id := fr.ID
	if id == "" {
		if cid, ok := callIDs[fr.Name]; ok {
			id = cid
		} else {
			id = fr.Name + "_0"
		}
	}
	chat.Messages = append(chat.Messages, engine.Message{
		Role:       engine.RoleTool,
		ToolCallID: id,
		ToolName:   fr.Name,
		Content:    content,
	})
}

// singleStringField returns v[key] as a string if resp is exactly {key: string}.
func singleStringField(resp map[string]any, key string) (string, bool) {
	if len(resp) != 1 {
		return "", false
	}
	s, ok := resp[key].(string)
	return s, ok
}

// --- Marshal ---

// Marshal converts a ChatRequest back into Gemini or Code Assist JSON.
func (a *Adapter) Marshal(chat *engine.ChatRequest) ([]byte, error) {
	codeAssist, _ := chat.ProviderExtensions[extCodeAssist].(bool)

	sys := buildSystemInstruction(chat.Messages)
	contents := buildContents(chat.Messages, codeAssist)
	tools := buildTools(chat.Tools)

	if codeAssist {
		return marshalCodeAssist(chat, sys, contents, tools)
	}

	gReq := geminiRequest{SystemInstruction: sys, Contents: contents, Tools: tools}
	if chat.MaxTokens != nil || chat.Temperature != nil || chat.TopP != nil || len(chat.StopSequences) > 0 {
		gReq.GenerationConfig = &geminiGenerationConfig{
			MaxOutputTokens: chat.MaxTokens,
			Temperature:     chat.Temperature,
			TopP:            chat.TopP,
			StopSequences:   chat.StopSequences,
		}
	}
	if len(chat.SafetySettings) > 0 {
		gReq.SafetySettings = chat.SafetySettings
	}

	b, err := json.Marshal(gReq)
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

// marshalCodeAssist rebuilds the wrapper, overlaying only the plugin-touched
// fields onto the preserved inner request and wrapper maps.
func marshalCodeAssist(chat *engine.ChatRequest, sys *geminiSystemInstruction, contents []geminiContent, tools []geminiTool) ([]byte, error) {
	inner := cloneMap(mapExt(chat.ProviderExtensions, extRequestExtra))
	inner["contents"] = contents
	if sys != nil {
		inner["systemInstruction"] = sys
	} else {
		delete(inner, "systemInstruction")
	}
	if len(tools) > 0 {
		inner["tools"] = tools
	} else {
		delete(inner, "tools")
	}

	wrapper := cloneMap(mapExt(chat.ProviderExtensions, extWrapper))
	wrapper["request"] = inner
	return json.Marshal(wrapper)
}

func buildSystemInstruction(msgs []engine.Message) *geminiSystemInstruction {
	var si *geminiSystemInstruction
	for _, msg := range msgs {
		if msg.Role == engine.RoleSystem {
			if si == nil {
				si = &geminiSystemInstruction{Role: "user"}
			}
			si.Parts = append(si.Parts, geminiPart{Text: msg.Content})
		}
	}
	return si
}

// buildContents reconstructs the Gemini contents array. When codeAssist is set
// it matches the Antigravity CLI's wire shape: functionCall and functionResponse
// each live in their own role:"model" content, with id + thoughtSignature.
func buildContents(msgs []engine.Message, codeAssist bool) []geminiContent {
	toolResultRole := "user"
	if codeAssist {
		toolResultRole = "model"
	}
	var out []geminiContent
	for _, msg := range msgs {
		switch msg.Role {
		case engine.RoleUser:
			if msg.Content != "" {
				out = append(out, geminiContent{Role: "user", Parts: []geminiPart{{Text: msg.Content}}})
			}
		case engine.RoleAssistant:
			if msg.Content != "" {
				p := geminiPart{Text: msg.Content}
				if msg.ThinkingSignature != "" {
					p.ThoughtSignature = msg.ThinkingSignature
				}
				out = append(out, geminiContent{Role: "model", Parts: []geminiPart{p}})
			}
			// Keep a turn's parallel tool calls in ONE content block, matching
			// how the model produced them: Gemini attaches a thoughtSignature to
			// only the first call of a parallel group, and splitting them into
			// separate blocks makes the server reject the signature-less ones
			// ("missing thought_signature", position N).
			if len(msg.ToolCalls) > 0 {
				parts := make([]geminiPart, 0, len(msg.ToolCalls))
				for _, tc := range msg.ToolCalls {
					parts = append(parts, geminiPart{
						ThoughtSignature: tc.Signature,
						FunctionCall:     &geminiFuncCall{Name: tc.Name, Args: tc.Arguments, ID: tc.ID},
					})
				}
				out = append(out, geminiContent{Role: "model", Parts: parts})
			}
		case engine.RoleTool:
			out = append(out, geminiContent{Role: toolResultRole, Parts: []geminiPart{{
				FunctionResponse: &geminiFuncResp{Name: msg.ToolName, ID: msg.ToolCallID, Response: toolResponseMap(msg.Content, codeAssist)},
			}}})
		}
	}
	return out
}

func buildTools(tools []engine.ToolDef) []geminiTool {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]geminiFuncDecl, 0, len(tools))
	for _, t := range tools {
		decls = append(decls, geminiFuncDecl{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return []geminiTool{{FunctionDeclarations: decls}}
}

// toolResponseMap rebuilds a functionResponse.response object from Content. When
// Content is a JSON object it is used verbatim; otherwise it is the raw tool
// text, re-wrapped as {"output": …} for Code Assist (matching agy's shape) or
// {"content": …} for bare Gemini.
func toolResponseMap(content string, codeAssist bool) map[string]any {
	var resp map[string]any
	if err := json.Unmarshal([]byte(content), &resp); err == nil {
		return resp
	}
	if codeAssist {
		return map[string]any{"output": content}
	}
	return map[string]any{"content": content}
}

func mapExt(ext map[string]any, key string) map[string]any {
	if ext == nil {
		return nil
	}
	m, _ := ext[key].(map[string]any)
	return m
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}
