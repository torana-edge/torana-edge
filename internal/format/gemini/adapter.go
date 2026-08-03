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
	"math"
	"strings"

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
	Role  string `json:"role"`
	Parts []any  `json:"parts"` // geminiPart (typed known arms) or json.RawMessage (unknown arms, raw projection)
}

// geminiWirePart pairs one wire part's typed decode with its RAW element.
// Unknown members that the typed struct does not declare survive via raw
// (inlineData, fileData, future arms) — the projection is lossless.
type geminiWirePart struct {
	part geminiPart
	raw  json.RawMessage
}

// UnmarshalJSON decodes every part TWICE: typed (the known-arm switch) and
// raw (the unknown-arm capture). A part that only the raw form can carry
// must never be reconstructed from the typed struct.
func (c *geminiContent) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role  string            `json:"role"`
		Parts []json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.Role = raw.Role
	c.Parts = make([]any, 0, len(raw.Parts))
	for _, p := range raw.Parts {
		var gp geminiPart
		if err := json.Unmarshal(p, &gp); err != nil {
			return err
		}
		c.Parts = append(c.Parts, geminiWirePart{part: gp, raw: p})
	}
	return nil
}

// geminiPart is a polymorphic content part. Code Assist may combine a
// thoughtSignature with a functionCall or text on the same part. Text is a
// pointer so an explicit empty text member (the trailing signature-only part's
// {"text":"","thoughtSignature":…}) is distinguishable from an absent one and
// survives marshal verbatim.
type geminiPart struct {
	Text             *string         `json:"text,omitempty"`
	Thought          bool            `json:"thought,omitempty"`
	ThoughtSignature string          `json:"thoughtSignature,omitempty"`
	FunctionCall     *geminiFuncCall `json:"functionCall,omitempty"`
	FunctionResponse *geminiFuncResp `json:"functionResponse,omitempty"`
}

// partText returns the part's text, treating an absent text member (nil
// pointer) as the empty string.
func partText(p geminiPart) string {
	if p.Text == nil {
		return ""
	}
	return *p.Text
}

type geminiFuncCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"` // raw lexemes, verbatim
	ID   string          `json:"id,omitempty"`
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
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // raw JSON Schema lexemes
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
	var err error

	chat := &engine.ChatRequest{Stream: false, Model: "gemini"}

	if gReq.GenerationConfig != nil && gReq.GenerationConfig.MaxOutputTokens != nil &&
		(*gReq.GenerationConfig.MaxOutputTokens < 1 || *gReq.GenerationConfig.MaxOutputTokens > math.MaxInt32) {
		return nil, fmt.Errorf("gemini: maxOutputTokens %d is outside 1..%d",
			*gReq.GenerationConfig.MaxOutputTokens, math.MaxInt32)
	}
	if gReq.GenerationConfig != nil {
		chat.MaxTokens = gReq.GenerationConfig.MaxOutputTokens
		chat.Temperature = gReq.GenerationConfig.Temperature
		chat.TopP = gReq.GenerationConfig.TopP
		chat.StopSequences = gReq.GenerationConfig.StopSequences
	}
	if len(gReq.SafetySettings) > 0 {
		safetyRaw, serr := json.Marshal(gReq.SafetySettings)
		if serr != nil {
			return nil, fmt.Errorf("gemini safety settings: %w", serr)
		}
		chat.SafetySettings, err = engine.ParseOptionalJSONArray(safetyRaw)
		if err != nil {
			return nil, fmt.Errorf("gemini safety settings: %w", err)
		}
	}

	if wrapped {
		// Code Assist envelope: HOST-ONLY topology. The wrapper (top-level
		// minus "request") and the inner request's non-rebuilt fields are
		// captured RAW; the sentinels are appended in fixed order.
		var wrapper, reqExtra engine.OptionalJSONObject
		topObj, toerr := engine.ParseOptionalJSONObject(rawBody)
		if toerr != nil {
			return nil, fmt.Errorf("gemini code assist extensions: %w", toerr)
		}
		wrapper, err = topObj.WithoutMembers("request")
		if err != nil {
			return nil, fmt.Errorf("gemini code assist extensions: %w", err)
		}
		innerObj, ierr := engine.ParseOptionalJSONObject(reqBytes)
		if ierr != nil {
			return nil, fmt.Errorf("gemini code assist extensions: %w", ierr)
		}
		reqExtra, err = innerObj.WithoutMembers("systemInstruction", "contents", "tools")
		if err != nil {
			return nil, fmt.Errorf("gemini code assist extensions: %w", err)
		}
		ext := engine.OptionalJSONObject{}
		if ext, err = ext.SetMember(extCodeAssist, json.RawMessage("true")); err == nil {
			ext, err = ext.SetMember(extWrapper, wrapper.Bytes())
		}
		if err == nil {
			ext, err = ext.SetMember(extRequestExtra, reqExtra.Bytes())
		}
		if err != nil {
			return nil, fmt.Errorf("gemini code assist extensions: %w", err)
		}
		chat.ProviderExtensions = ext
		if m, ok := readExtString(chat.ProviderExtensions, extWrapper+".model"); ok && m != "" {
			chat.Model = m
		}
	} else {
		// Bare Gemini: preserve unknown top-level fields deterministically
		// (original body minus the canonical fields, fixed delete order).
		ext, xerr := engine.ParseOptionalJSONObject(rawBody)
		if xerr != nil {
			return nil, fmt.Errorf("gemini provider extensions: %w", xerr)
		}
		ext, xerr = ext.WithoutMembers("systemInstruction", "contents", "tools", "generationConfig", "safetySettings")
		if xerr != nil {
			return nil, fmt.Errorf("gemini provider extensions: %w", xerr)
		}
		if ext, xerr = format.NormalizeExtensionObject(ext); xerr != nil {
			return nil, fmt.Errorf("gemini provider extensions: %w", xerr)
		}
		if !ext.IsAbsent() {
			chat.ProviderExtensions = ext
		}
	}

	// System instruction → RoleSystem message with ordered text blocks. The
	// system array is TEXT-ONLY (the parse-bypass seam): a non-text part
	// (functionCall, functionResponse, thought, unknown arms) would be a
	// silent fact drop, so it is refused at parse.
	if gReq.SystemInstruction != nil {
		msg := engine.Message{Role: engine.RoleSystem}
		for _, p := range gReq.SystemInstruction.Parts {
			if p.FunctionCall != nil || p.FunctionResponse != nil || p.Thought {
				return nil, fmt.Errorf("gemini: system instruction parts must be text only")
			}
			if partText(p) != "" {
				msg.Blocks = append(msg.Blocks, engine.Block{Text: &engine.TextBlock{Text: partText(p)}})
			}
		}
		if len(msg.Blocks) > 0 {
			chat.Messages = append(chat.Messages, msg)
		}
	}

	// Track synthesized IDs for tool results that lack an explicit id (bare Gemini).
	prevCallIdx := map[string]int{}
	callIDs := map[string]string{}

	for _, content := range gReq.Contents {
		msg, err := geminiContentToMessage(content, prevCallIdx, callIDs)
		if err != nil {
			return nil, err
		}
		if len(msg.Blocks) > 0 {
			chat.Messages = append(chat.Messages, msg)
		}
	}

	for _, tool := range gReq.Tools {
		for _, decl := range tool.FunctionDeclarations {
			params, err := engine.ParseRequiredObjectOrEmpty(decl.Parameters)
			if err != nil {
				return nil, fmt.Errorf("tool %q parameters: %w", decl.Name, err)
			}
			chat.Tools = append(chat.Tools, engine.ToolDef{
				Name:        decl.Name,
				Description: decl.Description,
				Parameters:  params,
			})
		}
	}

	return chat, nil
}

// geminiContentToMessage projects ONE wire content onto the ordered body:
// parts become blocks in wire order (text, thinking, tool use, tool result,
// unknown arms); functionResponse parts RIDE their message at their exact
// position (no synthetic RoleTool split). Signature topology rules:
//
//   - a thoughtSignature beside non-thought text is the content-bound token
//     on THAT text block;
//   - a signature on a thought:true part is the current-block token;
//   - a signature-only part with the EXPLICIT empty-text arm is the trailing
//     standalone (final; rejected without preceding covered content, or bare
//     without the text arm, or duplicated).
func geminiContentToMessage(content geminiContent, prevCallIdx map[string]int, callIDs map[string]string) (engine.Message, error) {
	msg := engine.Message{Role: mapRoleG(content.Role)}

	// Pre-pass: the trailing-standalone reject rules (leading / stranded /
	// bare / duplicate) and the mixed text/thinking-with-tool rejection
	// (Code Assist's wire shape cannot round-trip mixed contents).
	standalone := 0
	hasTextOrThinking := false
	hasTool := false
	for _, pAny := range content.Parts {
		p := pAny.(geminiWirePart).part
		// The provider-arm matrix: a gemini part carries at most ONE content
		// arm among {text (incl. the thought modifier), functionCall,
		// functionResponse}. A part with two is ambiguous — the projection
		// switch would pick one by precedence and silently drop the other —
		// so it is refused here, deterministically.
		arms := 0
		if p.Text != nil {
			arms++
		}
		if p.FunctionCall != nil {
			arms++
		}
		if p.FunctionResponse != nil {
			arms++
		}
		if arms > 1 {
			return msg, fmt.Errorf("gemini: part has %d content arms, want exactly one", arms)
		}
		switch {
		case p.FunctionCall != nil || p.FunctionResponse != nil:
			hasTool = true
		case p.Text != nil && *p.Text != "":
			hasTextOrThinking = true
		case p.Thought && p.Text != nil && *p.Text != "":
			hasTextOrThinking = true
		case p.ThoughtSignature != "" && !p.Thought && p.Text != nil && *p.Text == "":
			// The trailing-standalone shape only (explicit empty text,
			// signature, no thought flag): text/thinking-bound signatures
			// are per-block tokens, never counted here.
			standalone++
		}
	}
	if hasTextOrThinking && hasTool {
		return msg, fmt.Errorf("gemini: mixed text/thinking and tool-call parts in one content are not representable")
	}
	if standalone > 1 {
		return msg, fmt.Errorf("gemini: duplicate standalone signature")
	}

	seenCovered := false
	for i, pAny := range content.Parts {
		p := pAny.(geminiWirePart).part
		switch {
		case p.FunctionResponse != nil:
			tr, err := geminiToolResultBlock(p.FunctionResponse, callIDs)
			if err != nil {
				return msg, err
			}
			msg.Blocks = append(msg.Blocks, engine.Block{ToolResult: tr})
		case p.FunctionCall != nil:
			name := p.FunctionCall.Name
			id := p.FunctionCall.ID
			if id == "" {
				prevCallIdx[name]++
				id = fmt.Sprintf("%s_%d", name, prevCallIdx[name])
			}
			callIDs[name] = id
			args, err := engine.ParseRequiredObjectOrEmpty(p.FunctionCall.Args)
			if err != nil {
				return msg, fmt.Errorf("tool call %q args: %w", name, err)
			}
			msg.Blocks = append(msg.Blocks, engine.Block{ToolUse: &engine.ToolUseBlock{
				ID: id, Name: name, Arguments: args, Signature: p.ThoughtSignature,
			}})
		case p.Thought && p.Text != nil:
			msg.Blocks = append(msg.Blocks, engine.Block{Thinking: &engine.ThinkingBlock{
				Text: *p.Text, Signature: p.ThoughtSignature,
			}})
			if *p.Text != "" {
				seenCovered = true
			}
		case p.ThoughtSignature != "" && p.Text == nil:
			// A signature with NO text arm is not the supported trailing
			// shape — the TrailingStandalone contract requires the EXPLICIT
			// empty-text arm — so refuse instead of guessing at a binding.
			return msg, fmt.Errorf("gemini: standalone signature part must carry explicit empty text")
		case p.ThoughtSignature != "" && !p.Thought && p.Text != nil && *p.Text == "":
			// Trailing standalone: explicit empty-text arm, preceded by
			// covered content, singular, final. (Ordered before the plain
			// text case so the signature-only shape never becomes a text
			// block with a content-bound token.)
			if !seenCovered {
				if !hasTextOrThinking {
					return msg, fmt.Errorf("gemini: standalone signature on a tool-call-only turn")
				}
				return msg, fmt.Errorf("gemini: trailing signature without preceding text/thinking content")
			}
			if i != len(content.Parts)-1 {
				return msg, fmt.Errorf("gemini: standalone signature is not the final part")
			}
			msg.Blocks = append(msg.Blocks, engine.Block{TrailingSignature: &engine.TrailingSignatureBlock{Signature: p.ThoughtSignature}})
		case p.Text != nil:
			msg.Blocks = append(msg.Blocks, engine.Block{Text: &engine.TextBlock{
				Text: *p.Text, Signature: p.ThoughtSignature,
			}})
			if *p.Text != "" {
				seenCovered = true
			}
		default:
			// Unknown part arms ride the RAW element with the canonical
			// members removed (the projection invariant). The raw capture
			// keeps every member the typed struct does not declare —
			// inlineData, fileData, future arms — so the projection is
			// LOSSLESS; the typed struct is never re-marshaled here.
			raw := pAny.(geminiWirePart).raw
			payload, perr := stripGeminiPartFacts(raw)
			if perr != nil {
				return msg, fmt.Errorf("gemini part payload: %w", perr)
			}
			msg.Blocks = append(msg.Blocks, engine.Block{Unknown: &engine.UnknownBlock{Kind: "part", Payload: payload}})
		}
	}
	return msg, nil
}

// geminiToolResultBlock projects a functionResponse part onto a ToolResult
// block (the plain-text content; the Code Assist {"output":...} wrapping is
// a marshal-side provider shape).
func geminiToolResultBlock(fr *geminiFuncResp, callIDs map[string]string) (*engine.ToolResultBlock, error) {
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
	return &engine.ToolResultBlock{
		ToolCallID: id,
		ToolName:   fr.Name,
		Content:    []engine.ToolResultContentBlock{{Text: content}},
	}, nil
}

// stripGeminiPartFacts removes the known canonical members of a part object.
func stripGeminiPartFacts(raw json.RawMessage) (engine.RequiredJSONObject, error) {
	if len(raw) == 0 || raw[0] != '{' {
		return engine.RequiredJSONObject{}, fmt.Errorf("expected a JSON object part")
	}
	obj, err := engine.ParseRequiredJSONObject(raw)
	if err != nil {
		return obj, err
	}
	for _, k := range []string{"text", "thought", "thoughtSignature", "functionCall", "functionResponse"} {
		obj, err = obj.DeleteMember(k)
		if err != nil {
			return obj, err
		}
	}
	return obj, nil
}

// readExtString reads a dotted path (wrapper.member) from a raw extensions
// object; absent or non-string yields ok=false.
func readExtString(ext engine.OptionalJSONObject, path string) (string, bool) {
	if ext.IsAbsent() {
		return "", false
	}
	m, _, err := ext.DecodeObject()
	if err != nil {
		return "", false
	}
	parts := strings.SplitN(path, ".", 2)
	raw, ok := m[parts[0]]
	if !ok {
		return "", false
	}
	if len(parts) == 1 {
		var s string
		return s, json.Unmarshal(raw, &s) == nil
	}
	var inner map[string]json.RawMessage
	if json.Unmarshal(raw, &inner) != nil {
		return "", false
	}
	val, ok := inner[parts[1]]
	if !ok {
		return "", false
	}
	var s string
	return s, json.Unmarshal(val, &s) == nil
}

// mapRoleG maps gemini wire roles to engine roles.
func mapRoleG(role string) engine.Role {
	switch role {
	case "user":
		return engine.RoleUser
	case "model":
		return engine.RoleAssistant
	default:
		return engine.Role(role)
	}
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
	codeAssist := false
	if !chat.ProviderExtensions.IsAbsent() {
		if b, ok := readExtBool(chat.ProviderExtensions, extCodeAssist); ok {
			codeAssist = b
		}
	}

	sys := buildSystemInstruction(chat.Messages)
	contents, err := buildContents(chat.Messages, codeAssist)
	if err != nil {
		return nil, err
	}
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
	if !chat.SafetySettings.IsAbsent() {
		var ss []any
		if err := json.Unmarshal(chat.SafetySettings.Bytes(), &ss); err != nil {
			return nil, fmt.Errorf("gemini safety settings: %w", err)
		}
		gReq.SafetySettings = ss
	}

	b, err := json.Marshal(gReq)
	if err != nil {
		return nil, err
	}
	if !chat.ProviderExtensions.IsAbsent() {
		var outMap map[string]json.RawMessage
		if err := json.Unmarshal(b, &outMap); err != nil {
			return nil, err
		}
		if err := format.MergeRawMembers(outMap, chat.ProviderExtensions.Bytes()); err != nil {
			return nil, fmt.Errorf("gemini provider extensions merge: %w", err)
		}
		return json.Marshal(outMap)
	}
	return b, nil
}

// marshalCodeAssist rebuilds the envelope from the raw captures: the inner
// request keeps every preserved member byte-exact; only the IR-rebuilt
// fields (contents/systemInstruction/tools) are overlaid.
func marshalCodeAssist(chat *engine.ChatRequest, sys *geminiSystemInstruction, contents []geminiContent, tools []geminiTool) ([]byte, error) {
	ext, _, err := chat.ProviderExtensions.DecodeObject()
	if err != nil {
		return nil, fmt.Errorf("code assist extensions: %w", err)
	}
	inner, err := engine.ParseOptionalJSONObject(ext[extRequestExtra])
	if err != nil {
		return nil, fmt.Errorf("code assist extensions: %w", err)
	}
	contentsBytes, err := json.Marshal(contents)
	if err != nil {
		return nil, err
	}
	if inner, err = inner.SetMember("contents", contentsBytes); err != nil {
		return nil, err
	}
	if sys != nil {
		sysBytes, serr := json.Marshal(sys)
		if serr != nil {
			return nil, serr
		}
		if inner, err = inner.SetMember("systemInstruction", sysBytes); err != nil {
			return nil, err
		}
	} else if inner, err = inner.DeleteMember("systemInstruction"); err != nil {
		return nil, err
	}
	if len(tools) > 0 {
		toolsBytes, terr := json.Marshal(tools)
		if terr != nil {
			return nil, terr
		}
		if inner, err = inner.SetMember("tools", toolsBytes); err != nil {
			return nil, err
		}
	} else if inner, err = inner.DeleteMember("tools"); err != nil {
		return nil, err
	}

	wrapper, err := engine.ParseOptionalJSONObject(ext[extWrapper])
	if err != nil {
		return nil, fmt.Errorf("code assist extensions: %w", err)
	}
	if wrapper, err = wrapper.SetMember("request", inner.Bytes()); err != nil {
		return nil, err
	}
	return wrapper.Bytes(), nil
}

// readExtBool reads a bool member from a raw extensions object.
func readExtBool(ext engine.OptionalJSONObject, key string) (bool, bool) {
	if ext.IsAbsent() {
		return false, false
	}
	m, _, err := ext.DecodeObject()
	if err != nil {
		return false, false
	}
	raw, ok := m[key]
	if !ok {
		return false, false
	}
	var b bool
	return b, json.Unmarshal(raw, &b) == nil
}

// geminiCanonicalPartMembers are the members the block kinds own; an
// Unknown part payload must never carry them (the projection invariant,
// enforced at marshal exactly like the other adapters).
var geminiCanonicalPartMembers = []string{"text", "thought", "thoughtSignature", "functionCall", "functionResponse"}

func rejectGeminiProjection(u *engine.UnknownBlock) error {
	payload, _, err := u.Payload.DecodeObject()
	if err != nil {
		return fmt.Errorf("gemini: unknown part payload: %w", err)
	}
	for _, canon := range geminiCanonicalPartMembers {
		if _, dup := payload[canon]; dup {
			return fmt.Errorf("gemini: unknown part payload duplicates canonical member %q (projection invariant)", canon)
		}
	}
	return nil
}

func buildSystemInstruction(msgs []engine.Message) *geminiSystemInstruction {
	var si *geminiSystemInstruction
	for _, msg := range msgs {
		if msg.Role == engine.RoleSystem {
			if si == nil {
				si = &geminiSystemInstruction{Role: "user"}
			}
			for _, b := range msg.Blocks {
				if b.Text != nil {
					si.Parts = append(si.Parts, geminiPart{Text: new(b.Text.Text)})
				}
			}
		}
	}
	return si
}

// buildContents reconstructs the Gemini contents array from the ordered
// body. When codeAssist is set it matches the Antigravity CLI's wire shape:
// functionCall and functionResponse each live in their own role:"model"
// content, with id + thoughtSignature. Unrepresentable block kinds fail
// closed: cache breakpoints, redacted thinking, trailing signatures
// (bare Gemini), and unknown arms outside a content part.
func buildContents(msgs []engine.Message, codeAssist bool) ([]geminiContent, error) {
	toolResultRole := "user"
	if codeAssist {
		toolResultRole = "model"
	}
	var out []geminiContent
	for _, msg := range msgs {
		switch msg.Role {
		case engine.RoleUser, engine.RoleSystem:
			content := geminiContent{Role: "user"}
			for _, b := range msg.Blocks {
				switch {
				case b.Text != nil:
					content.Parts = append(content.Parts, geminiPart{Text: new(b.Text.Text)})
				case b.Unknown != nil:
					// Unknown part arms are re-emitted RAW at their exact
					// position (the projection invariant is enforced: the
					// payload must not carry a canonical member a plugin
					// could have injected).
					if err := rejectGeminiProjection(b.Unknown); err != nil {
						return nil, err
					}
					content.Parts = append(content.Parts, json.RawMessage(b.Unknown.Payload.Bytes()))
				case b.ToolResult != nil:
					text := ""
					for _, c := range b.ToolResult.Content {
						if c.Unknown != nil || c.CacheBreakpoint != nil {
							return nil, fmt.Errorf("gemini: structured tool-result content is not representable")
						}
						text += c.Text
					}
					content.Parts = append(content.Parts, geminiPart{
						FunctionResponse: &geminiFuncResp{
							Name: b.ToolResult.ToolName, ID: b.ToolResult.ToolCallID,
							Response: toolResponseMap(text, codeAssist),
						},
					})
				default:
					return nil, fmt.Errorf("gemini: block kind not representable in a %s content", msg.Role)
				}
			}
			if len(content.Parts) > 0 {
				out = append(out, content)
			}
		case engine.RoleAssistant:
			// Split the ordered body into contents per the Gemini wire
			// contract: text/thinking contents, then tool-call contents
			// (parallel calls in ONE content, matching the model's output).
			var textContent *geminiContent
			var toolParts []any
			for _, b := range msg.Blocks {
				switch {
				case b.Text != nil:
					if textContent == nil {
						textContent = &geminiContent{Role: "model"}
					}
					textContent.Parts = append(textContent.Parts, geminiPart{Text: new(b.Text.Text), ThoughtSignature: b.Text.Signature})
				case b.Thinking != nil:
					if textContent == nil {
						textContent = &geminiContent{Role: "model"}
					}
					textContent.Parts = append(textContent.Parts, geminiPart{
						Thought: true, Text: new(b.Thinking.Text), ThoughtSignature: b.Thinking.Signature,
					})
				case b.Unknown != nil:
					if err := rejectGeminiProjection(b.Unknown); err != nil {
						return nil, err
					}
					if textContent == nil {
						textContent = &geminiContent{Role: "model"}
					}
					textContent.Parts = append(textContent.Parts, json.RawMessage(b.Unknown.Payload.Bytes()))
				case b.ToolUse != nil:
					toolParts = append(toolParts, geminiPart{
						ThoughtSignature: b.ToolUse.Signature,
						FunctionCall:     &geminiFuncCall{Name: b.ToolUse.Name, Args: b.ToolUse.Arguments.Bytes(), ID: b.ToolUse.ID},
					})
				case b.ToolResult != nil:
					text := ""
					for _, c := range b.ToolResult.Content {
						if c.Unknown != nil || c.CacheBreakpoint != nil {
							return nil, fmt.Errorf("gemini: structured tool-result content is not representable")
						}
						text += c.Text
					}
					out = append(out, geminiContent{Role: toolResultRole, Parts: []any{geminiPart{
						FunctionResponse: &geminiFuncResp{
							Name: b.ToolResult.ToolName, ID: b.ToolResult.ToolCallID,
							Response: toolResponseMap(text, codeAssist),
						},
					}}})
				case b.TrailingSignature != nil:
					// The trailing signature-only part is a plain Gemini
					// part (explicit empty text + thoughtSignature), NOT an
					// envelope artifact — it round-trips on the unwrapped
					// shape exactly as unmarshal parsed it.
					if textContent == nil {
						textContent = &geminiContent{Role: "model"}
					}
					textContent.Parts = append(textContent.Parts, geminiPart{Text: new(""), ThoughtSignature: b.TrailingSignature.Signature})
				default:
					return nil, fmt.Errorf("gemini: block kind not representable in an assistant content")
				}
			}
			if textContent != nil {
				out = append(out, *textContent)
			}
			if len(toolParts) > 0 {
				out = append(out, geminiContent{Role: "model", Parts: toolParts})
			}
		case engine.RoleTool:
			// A tool-role message is only representable when it carries one
			// tool result; emit it in the tool-result content shape.
			for _, b := range msg.Blocks {
				if b.ToolResult == nil {
					return nil, fmt.Errorf("gemini: tool-role message with a non-tool-result block")
				}
				text := ""
				for _, c := range b.ToolResult.Content {
					if c.Unknown != nil || c.CacheBreakpoint != nil {
						return nil, fmt.Errorf("gemini: structured tool-result content is not representable")
					}
					text += c.Text
				}
				out = append(out, geminiContent{Role: toolResultRole, Parts: []any{geminiPart{
					FunctionResponse: &geminiFuncResp{
						Name: b.ToolResult.ToolName, ID: b.ToolResult.ToolCallID,
						Response: toolResponseMap(text, codeAssist),
					},
				}}})
			}
		default:
			return nil, fmt.Errorf("gemini: unmodelled message role %q", msg.Role)
		}
	}
	return out, nil
}
func buildTools(tools []engine.ToolDef) []geminiTool {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]geminiFuncDecl, 0, len(tools))
	for _, t := range tools {
		decls = append(decls, geminiFuncDecl{Name: t.Name, Description: t.Description, Parameters: t.Parameters.Bytes()})
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
