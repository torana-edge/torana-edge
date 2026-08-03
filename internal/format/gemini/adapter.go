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
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/engine/pbconv"
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

// The Code Assist envelope is PROVIDER-VISIBLE (checkpoint revision 4,
// class A): provider_extensions_json holds the original wire object with
// the canonical ABI-owned members projected out — top level = outer-
// wrapper extras, the `request` member = inner-request extras (with
// generationConfig's canonical members stripped). The VARIANT fact
// (Code-Assist-wrapped vs bare) is typed host-only topology (class B):
// engine.ChatRequest.CodeAssist.
const envelopeRequestMember = "request"

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

// UnmarshalJSON applies the same Part grammar to the system array, then
// enforces the system seam: a system part must be a PLAIN text arm (no
// thought, no signature, no function/media/future arms) — anything Torana
// cannot represent in the system array is the value-free 400, never a
// silent drop.
func (si *geminiSystemInstruction) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role  string            `json:"role"`
		Parts []json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	si.Role = raw.Role
	for _, p := range raw.Parts {
		arm, err := validateGeminiPartRaw(p, "system")
		if err != nil {
			return err
		}
		if arm != "text" {
			return fmt.Errorf("gemini: system instruction parts must be text only (part arm %q)", arm)
		}
		var gp geminiPart
		if err := json.Unmarshal(p, &gp); err != nil {
			return err
		}
		if gp.Thought || gp.ThoughtSignature != "" {
			return fmt.Errorf("gemini: system instruction parts must be plain text (no thought or signature)")
		}
		si.Parts = append(si.Parts, gp)
	}
	return nil
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
		// The executable Part grammar over the RAW members — a typed decode
		// cannot see facts the grammar must refuse (text+inlineData, two
		// media arms, a modifier without its carrier).
		if _, err := validateGeminiPartRaw(p, "content"); err != nil {
			return err
		}
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
// survives marshal verbatim. PartMetadata is RAW so provider partMetadata
// (legal on ANY part) round-trips lexeme-exact into part_metadata_json.
type geminiPart struct {
	Text             *string         `json:"text,omitempty"`
	Thought          bool            `json:"thought,omitempty"`
	ThoughtSignature string          `json:"thoughtSignature,omitempty"`
	FunctionCall     *geminiFuncCall `json:"functionCall,omitempty"`
	FunctionResponse *geminiFuncResp `json:"functionResponse,omitempty"`
	PartMetadata     json.RawMessage `json:"partMetadata,omitempty"`
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
	Name string `json:"name"`
	// Response is the RAW response object bytes — the SINGLE authority for
	// the response fact (REV 4 §5): the ToolResultBlock's FIRST Text
	// element carries these exact bytes; marshal re-emits them verbatim
	// when they are a strict JSON object.
	Response     json.RawMessage      `json:"response"`
	ID           string               `json:"id,omitempty"`
	WillContinue *bool                `json:"willContinue,omitempty"`
	Scheduling   *string              `json:"scheduling,omitempty"`
	Parts        []geminiFuncRespPart `json:"parts,omitempty"`
}

// geminiFuncRespPart is the sealed FunctionResponsePart union (exactly one
// arm; top-level Part ancillaries are NOT legal inside).
type geminiFuncRespPart struct {
	InlineData *geminiFuncRespBlob `json:"inlineData,omitempty"`
	FileData   *geminiFuncRespFile `json:"fileData,omitempty"`
}

type geminiFuncRespBlob struct {
	MimeType    string `json:"mimeType"`
	Data        string `json:"data"`
	DisplayName string `json:"displayName,omitempty"`
}

type geminiFuncRespFile struct {
	MimeType    string `json:"mimeType"`
	FileURI     string `json:"fileUri"`
	DisplayName string `json:"displayName,omitempty"`
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
		// Code Assist: the variant is TYPED host-only topology; the
		// envelope is PROVIDER-VISIBLE (the original wire object with
		// canonical members projected out, checkpoint 4b): top level =
		// outer-wrapper extras (model is FORBIDDEN as an extra — it is
		// rebuilt from ChatRequest.Model), the `request` member =
		// inner-request extras (systemInstruction/contents/tools/
		// safetySettings and generationConfig's canonical members are
		// FORBIDDEN as extras — rebuilt from canonical ABI fields; unknown
		// generation siblings survive inside the preserved generationConfig).
		chat.CodeAssist = true
		topObj, toerr := engine.ParseOptionalJSONObject(rawBody)
		if toerr != nil {
			return nil, fmt.Errorf("gemini code assist extensions: %w", toerr)
		}
		env, err := topObj.WithoutMembers(envelopeRequestMember)
		if err != nil {
			return nil, fmt.Errorf("gemini code assist extensions: %w", err)
		}
		// Outer model is canonical: rebuilt from ChatRequest.Model, never
		// a preserved extra.
		modelRaw := readExtRaw(env, "model")
		hasModel := len(modelRaw) > 0
		env, err = env.DeleteMember("model")
		if err != nil {
			return nil, fmt.Errorf("gemini code assist extensions: %w", err)
		}
		innerObj, ierr := engine.ParseOptionalJSONObject(reqBytes)
		if ierr != nil {
			return nil, fmt.Errorf("gemini code assist extensions: %w", ierr)
		}
		reqExtra, err := innerObj.WithoutMembers("systemInstruction", "contents", "tools", "safetySettings")
		if err != nil {
			return nil, fmt.Errorf("gemini code assist extensions: %w", err)
		}
		if reqExtra, err = stripGenerationCanonicalMembers(reqExtra); err != nil {
			return nil, fmt.Errorf("gemini code assist extensions: %w", err)
		}
		if env, err = env.SetMember(envelopeRequestMember, reqExtra.Bytes()); err != nil {
			return nil, fmt.Errorf("gemini code assist extensions: %w", err)
		}
		chat.ProviderExtensions = env
		if hasModel {
			var m string
			if json.Unmarshal(modelRaw, &m) == nil && m != "" {
				chat.Model = m
			}
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
			// The grammar ran at decode: the arm is text and the part is
			// plain (no thought/signature); the projection is the text.
			// EXPLICIT EMPTY text is a first-class arm (its position and
			// metadata survive) and partMetadata is a typed carrier on
			// system text too.
			pm, err := partMetadata(p.PartMetadata)
			if err != nil {
				return nil, err
			}
			msg.Blocks = append(msg.Blocks, engine.Block{Text: &engine.TextBlock{
				Text: partText(p), PartMetadataJson: pm,
			}})
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
		// The executable Part grammar already ran over the RAW members at
		// decode (validateGeminiPartRaw): exactly one arm, documented
		// modifier combinations only. The typed switch below therefore has
		// exactly one arm to choose.
		p := pAny.(geminiWirePart).part
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
			tr.Signature = p.ThoughtSignature
			pm, err := partMetadata(p.PartMetadata)
			if err != nil {
				return msg, err
			}
			tr.PartMetadataJson = pm
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
			pm, err := partMetadata(p.PartMetadata)
			if err != nil {
				return msg, err
			}
			msg.Blocks = append(msg.Blocks, engine.Block{ToolUse: &engine.ToolUseBlock{
				ID: id, Name: name, Arguments: args, Signature: p.ThoughtSignature, PartMetadataJson: pm,
			}})
		case p.Thought && p.Text != nil:
			pm, err := partMetadata(p.PartMetadata)
			if err != nil {
				return msg, err
			}
			msg.Blocks = append(msg.Blocks, engine.Block{Thinking: &engine.ThinkingBlock{
				Text: *p.Text, Signature: p.ThoughtSignature, PartMetadataJson: pm,
			}})
			if *p.Text != "" {
				seenCovered = true
			}
		case p.ThoughtSignature != "" && p.Text == nil && geminiRawHasText(pAny.(geminiWirePart).raw):
			// A signature with NO TEXT ARM on a part that still HAS an
			// explicit text member is not the supported trailing shape —
			// the TrailingStandalone contract requires the EXPLICIT
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
			pm, err := partMetadata(p.PartMetadata)
			if err != nil {
				return msg, err
			}
			msg.Blocks = append(msg.Blocks, engine.Block{TrailingSignature: &engine.TrailingSignatureBlock{
				Signature: p.ThoughtSignature, PartMetadataJson: pm,
			}})
		case p.Text != nil:
			pm, err := partMetadata(p.PartMetadata)
			if err != nil {
				return msg, err
			}
			msg.Blocks = append(msg.Blocks, engine.Block{Text: &engine.TextBlock{
				Text: *p.Text, Signature: p.ThoughtSignature, PartMetadataJson: pm,
			}})
			if *p.Text != "" {
				seenCovered = true
			}
		default:
			// Unknown part arms ride the RAW element with the canonical
			// members removed (the projection invariant). The raw capture
			// keeps every member the typed struct does not declare —
			// inlineData, fileData, future arms, videoMetadata,
			// mediaResolution — so the projection is LOSSLESS; the typed
			// struct is never re-marshaled here. The signature and
			// partMetadata facts are typed carriers on the Unknown block.
			raw := pAny.(geminiWirePart).raw
			payload, perr := stripGeminiPartFacts(raw)
			if perr != nil {
				return msg, fmt.Errorf("gemini part payload: %w", perr)
			}
			pm, err := partMetadata(p.PartMetadata)
			if err != nil {
				return msg, err
			}
			msg.Blocks = append(msg.Blocks, engine.Block{Unknown: &engine.UnknownBlock{
				Kind: "part", Payload: payload, PartMetadataJson: pm, Signature: p.ThoughtSignature,
			}})
		}
	}
	return msg, nil
}

// geminiToolResultBlock projects a functionResponse part onto a ToolResult
// block per the REV 4 §5 SINGLE-AUTHORITY model: the response object is
// the FIRST content element — a Text element carrying the RAW response
// object bytes (lexeme-exact, never decoded and re-encoded); subsequent
// ordered parts become nested Unknown media elements (the sealed
// FunctionResponsePart union, preserved verbatim); willContinue/scheduling
// are typed presence-aware carriers; thoughtSignature is the block token.
// Marshal failure of the raw response is an error, never {}.
func geminiToolResultBlock(fr *geminiFuncResp, callIDs map[string]string) (*engine.ToolResultBlock, error) {
	id := fr.ID
	if id == "" {
		if cid, ok := callIDs[fr.Name]; ok {
			id = cid
		} else {
			id = fr.Name + "_0"
		}
	}
	content := []engine.ToolResultContentBlock{{
		Text: string(fr.Response), // the raw object bytes ARE the text
	}}
	for _, p := range fr.Parts {
		payload, err := geminiFuncRespPartPayload(p)
		if err != nil {
			return nil, err
		}
		content = append(content, engine.ToolResultContentBlock{Unknown: &engine.UnknownBlock{Kind: "part", Payload: payload}})
	}
	tr := &engine.ToolResultBlock{
		ToolCallID: id,
		ToolName:   fr.Name,
		Content:    content,
	}
	if fr.WillContinue != nil {
		v := *fr.WillContinue
		tr.WillContinue = &v
	}
	if fr.Scheduling != nil {
		v := *fr.Scheduling
		tr.Scheduling = &v
	}
	return tr, nil
}

// geminiFuncRespPartPayload preserves a sealed FunctionResponsePart element
// as the exact one-arm WRAPPER object (the approved authority shape):
// {"inlineData":{...}} or {"fileData":{...}}.
func geminiFuncRespPartPayload(p geminiFuncRespPart) (engine.RequiredJSONObject, error) {
	var wrapper map[string]any
	switch {
	case p.InlineData != nil && p.FileData == nil:
		wrapper = map[string]any{"inlineData": p.InlineData}
	case p.FileData != nil && p.InlineData == nil:
		wrapper = map[string]any{"fileData": p.FileData}
	default:
		return engine.RequiredJSONObject{}, fmt.Errorf("gemini: FunctionResponsePart must have exactly one arm")
	}
	raw, err := json.Marshal(wrapper)
	if err != nil {
		return engine.RequiredJSONObject{}, err
	}
	// The sealed-union validator runs over the wrapper BEFORE it becomes a
	// payload, so an unrepresentable shape never reaches the engine.
	if err := validateGeminiFuncRespPart(raw, "marshal"); err != nil {
		return engine.RequiredJSONObject{}, err
	}
	return engine.ParseRequiredJSONObject(raw)
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
	for _, k := range geminiCanonicalPartMembers {
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
	// The owning validation at EVERY marshal entry: the engine pointer sum
	// must be in the closed domain before any arm is projected — a future
	// call site cannot bypass the checked boundary by accident.
	if err := pbconv.ValidateFullRequest(chat); err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}
	codeAssist := chat.CodeAssist

	sys, err := buildSystemInstruction(chat.Messages)
	if err != nil {
		return nil, err
	}
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

// marshalCodeAssist rebuilds the Code Assist wire from the typed facts and
// the PROVIDER-VISIBLE envelope (checkpoint 4b):
//
//   - OUTPUT VERIFICATION first: the replacement envelope must not smuggle
//     canonical members through the extras path (outer model; inner
//     systemInstruction/contents/tools/safetySettings; generationConfig
//     canonical members) — a plugin that did is refused, never silently
//     ignored;
//   - outer `model` is rebuilt from ChatRequest.Model (canonical wins);
//   - inner `generationConfig` is rebuilt: canonical members from the PB
//     request, unknown sibling members preserved losslessly from the
//     envelope's inner scope;
//   - inner `safetySettings` rebuilt from safety_settings_json;
//   - systemInstruction/contents/tools rebuilt from their canonical fields;
//   - every other wrapper/inner extra passes through from the envelope at
//     its exact wire scope.
func marshalCodeAssist(chat *engine.ChatRequest, sys *geminiSystemInstruction, contents []geminiContent, tools []geminiTool) ([]byte, error) {
	// PURE: a LOCAL envelope value is built; the input chat is never
	// mutated (marshal must not change the request/cache identity).
	// Only an ABSENT envelope may locally default to the documented empty
	// envelope; a PRESENT envelope missing the structural `request` member
	// is REFUSED (the grammar says request is an object).
	env := chat.ProviderExtensions
	if env.IsAbsent() {
		// The ABSENT envelope defaults to the documented EMPTY envelope
		// (with its structural request member); a PRESENT envelope missing
		// `request` is refused below.
		empty, perr := engine.ParseOptionalJSONObject([]byte(`{"request":{}}`))
		if perr != nil {
			return nil, perr
		}
		env = empty
	}
	if _, _, err := env.DecodeObject(); err != nil {
		return nil, fmt.Errorf("code assist extensions: %w", err)
	}
	if raw := readExtRaw(env, envelopeRequestMember); raw == nil {
		return nil, fmt.Errorf("code assist envelope: present envelope missing the structural %q member", envelopeRequestMember)
	}
	if err := verifyCodeAssistEnvelope(env); err != nil {
		return nil, err
	}
	ext, _, err := env.DecodeObject()
	if err != nil {
		return nil, fmt.Errorf("code assist extensions: %w", err)
	}

	// Inner scope: envelope extras + canonical rebuilds (the local env).
	reqExtra, err := engine.ParseOptionalJSONObject(ext[envelopeRequestMember])
	if err != nil {
		return nil, fmt.Errorf("code assist extensions: %w", err)
	}
	if !chat.SafetySettings.IsAbsent() {
		if reqExtra, err = reqExtra.SetMember("safetySettings", chat.SafetySettings.Bytes()); err != nil {
			return nil, err
		}
	} else if reqExtra, err = reqExtra.DeleteMember("safetySettings"); err != nil {
		return nil, err
	}
	if reqExtra, err = rebuildGenerationConfig(chat, reqExtra); err != nil {
		return nil, err
	}
	contentsBytes, err := json.Marshal(contents)
	if err != nil {
		return nil, err
	}
	if reqExtra, err = reqExtra.SetMember("contents", contentsBytes); err != nil {
		return nil, err
	}
	if sys != nil {
		sysBytes, serr := json.Marshal(sys)
		if serr != nil {
			return nil, serr
		}
		if reqExtra, err = reqExtra.SetMember("systemInstruction", sysBytes); err != nil {
			return nil, err
		}
	} else if reqExtra, err = reqExtra.DeleteMember("systemInstruction"); err != nil {
		return nil, err
	}
	if len(tools) > 0 {
		toolsBytes, terr := json.Marshal(tools)
		if terr != nil {
			return nil, terr
		}
		if reqExtra, err = reqExtra.SetMember("tools", toolsBytes); err != nil {
			return nil, err
		}
	} else if reqExtra, err = reqExtra.DeleteMember("tools"); err != nil {
		return nil, err
	}

	// Outer scope: envelope extras + canonical model.
	outer, err := env.DeleteMember(envelopeRequestMember)
	if err != nil {
		return nil, err
	}
	modelBytes, merr := json.Marshal(chat.Model)
	if merr != nil {
		return nil, merr
	}
	if outer, err = outer.SetMember("model", modelBytes); err != nil {
		return nil, err
	}
	if outer, err = outer.SetMember(envelopeRequestMember, reqExtra.Bytes()); err != nil {
		return nil, err
	}
	return outer.Bytes(), nil
}

// verifyCodeAssistEnvelope enforces the 4b grammar on a (possibly
// plugin-replaced) envelope: canonical members are FORBIDDEN as extras at
// both scopes and in generationConfig.
func verifyCodeAssistEnvelope(env engine.OptionalJSONObject) error {
	m, _, err := env.DecodeObject()
	if err != nil {
		return fmt.Errorf("code assist envelope: %w", err)
	}
	for _, k := range []string{"model"} {
		if _, ok := m[k]; ok {
			return fmt.Errorf("code assist envelope: canonical outer member %q smuggled through the extras path", k)
		}
	}
	innerRaw, ok := m[envelopeRequestMember]
	if !ok {
		return fmt.Errorf("code assist envelope: missing %q structural member", envelopeRequestMember)
	}
	// The strict REQUIRED-object authority: null, arrays, scalars, and
	// malformed text are classified errors, never panics, never a nil map.
	inner, err := engine.ParseRequiredJSONObject(innerRaw)
	if err != nil {
		return fmt.Errorf("code assist envelope: %q must be a strict JSON object: %w", envelopeRequestMember, err)
	}
	innerMap, _, err := inner.DecodeObject()
	if err != nil {
		return fmt.Errorf("code assist envelope: %q: %w", envelopeRequestMember, err)
	}
	for _, k := range []string{"systemInstruction", "contents", "tools", "safetySettings"} {
		if _, ok := innerMap[k]; ok {
			return fmt.Errorf("code assist envelope: canonical inner member %q smuggled through the extras path", k)
		}
	}
	if gcRaw, ok := innerMap["generationConfig"]; ok {
		gc, err := engine.ParseRequiredJSONObject(gcRaw)
		if err != nil {
			return fmt.Errorf("code assist envelope: generationConfig must be a strict JSON object: %w", err)
		}
		gcMap, _, err := gc.DecodeObject()
		if err != nil {
			return fmt.Errorf("code assist envelope: generationConfig: %w", err)
		}
		for _, k := range []string{"maxOutputTokens", "temperature", "topP", "stopSequences"} {
			if _, ok := gcMap[k]; ok {
				return fmt.Errorf("code assist envelope: canonical generationConfig member %q smuggled through the extras path", k)
			}
		}
	}
	return nil
}

// rebuildGenerationConfig overlays the canonical members from the PB
// request onto the preserved generationConfig siblings.
func rebuildGenerationConfig(chat *engine.ChatRequest, inner engine.OptionalJSONObject) (engine.OptionalJSONObject, error) {
	// SPAN-OPERATION based (finding 4): the preserved generationConfig is
	// rebuilt via DeleteMember/SetMember on the raw object wrapper so
	// unknown sibling members retain their EXACT lexemes (order,
	// whitespace, numeric spellings, escapes, nested bytes).
	var gc engine.OptionalJSONObject
	var err error
	if !inner.IsAbsent() {
		m, _, err := inner.DecodeObject()
		if err != nil {
			return inner, err
		}
		if raw, ok := m["generationConfig"]; ok {
			parsed, err := engine.ParseOptionalJSONObject(raw)
			if err != nil {
				return inner, fmt.Errorf("code assist extensions: generationConfig must be a strict JSON object: %w", err)
			}
			gc = parsed
		}
	}
	// Overlay canonical members (delete absent ones, set present ones) —
	// span ops keep the unknown siblings byte-exact.
	canonical := []string{"maxOutputTokens", "temperature", "topP", "stopSequences"}
	for _, k := range canonical {
		if !gc.IsAbsent() {
			m, _, derr := gc.DecodeObject()
			if derr != nil {
				return inner, derr
			}
			if _, ok := m[k]; ok {
				gc, derr = gc.DeleteMember(k)
				if derr != nil {
					return inner, derr
				}
			}
		}
	}
	set := func(k string, v any) (engine.OptionalJSONObject, error) {
		b, merr := json.Marshal(v)
		if merr != nil {
			return gc, merr
		}
		return gc.SetMember(k, b)
	}
	if chat.MaxTokens != nil {
		if gc, err = set("maxOutputTokens", *chat.MaxTokens); err != nil {
			return inner, err
		}
	}
	if chat.Temperature != nil {
		if gc, err = set("temperature", *chat.Temperature); err != nil {
			return inner, err
		}
	}
	if chat.TopP != nil {
		if gc, err = set("topP", *chat.TopP); err != nil {
			return inner, err
		}
	}
	if len(chat.StopSequences) > 0 {
		if gc, err = set("stopSequences", chat.StopSequences); err != nil {
			return inner, err
		}
	}
	if gc.IsAbsent() {
		return inner.DeleteMember("generationConfig")
	}
	return inner.SetMember("generationConfig", gc.Bytes())
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

// geminiAncillaryPartMembers are the DOCUMENTED part modifiers (REV 4 §1):
// thought (marks a text arm as the thinking arm), thoughtSignature (the
// provenance token — legal on ANY arm per the provider guidance), and
// partMetadata (provider custom metadata, legal on ANY part). The media
// ancillaries videoMetadata/mediaResolution are legal ONLY on media arms
// (inlineData/fileData) — enforced by validateGeminiPartRaw. Every other
// member is an ARM member.
var geminiAncillaryPartMembers = map[string]bool{
	"thought":          true,
	"thoughtSignature": true,
	"partMetadata":     true,
}

// geminiMediaAncillaryMembers are the media-only part modifiers.
var geminiMediaAncillaryMembers = map[string]bool{
	"videoMetadata":   true,
	"mediaResolution": true,
}

// geminiCanonicalPartMembers are the members the block kinds own; an
// Unknown part payload must never carry them (the projection invariant,
// enforced at marshal exactly like the other adapters). partMetadata is a
// typed carrier now; videoMetadata/mediaResolution stay INSIDE the media
// unknown payload (preserved verbatim, never narrowed).
var geminiCanonicalPartMembers = []string{"text", "thought", "thoughtSignature", "functionCall", "functionResponse", "partMetadata"}

// geminiSchedulingVocabulary is the usable functionResponse scheduling
// vocabulary (pinned from the vendored provider snapshot). An unknown or
// UNSPECIFIED value is the value-free 400 — never a silent default;
// absence is the provider default WHEN_IDLE and is distinct.
var geminiSchedulingVocabulary = map[string]bool{
	"SILENT":    true,
	"WHEN_IDLE": true,
	"INTERRUPT": true,
}

// geminiSchedulingValues is the canonical spelling order.
var geminiSchedulingValues = []string{"SILENT", "WHEN_IDLE", "INTERRUPT"}

// validateGeminiPartRaw is the executable Gemini Part grammar, over the RAW
// member set (deterministic — counting and membership are
// order-independent, so a map-iteration flip can never change the verdict):
//
//   - exactly ONE arm member: {text, functionCall, functionResponse,
//     inlineData, fileData, toolCall, toolResponse, or any future arm};
//     zero arms (a part of only modifiers) or two+ arms (text+inlineData,
//     inlineData+fileData, ...) are ambiguous and refused;
//   - thought is legal ONLY on a text arm (the thinking arm);
//   - thoughtSignature is legal on ANY arm (provider guidance: the
//     signature binds the complete signed part — text, thinking, tool
//     calls, tool results, media, and future arms);
//   - partMetadata is legal on ANY part;
//   - videoMetadata/mediaResolution are legal ONLY on the media arms
//     (inlineData/fileData);
//   - a functionResponse arm is additionally governed by the
//     FunctionResponse grammar: willContinue (bool), scheduling (the exact
//     vocabulary; unknown/UNSPECIFIED -> value-free 400), and a SEALED
//     parts union (FunctionResponsePart: exactly one of inlineData/fileData
//     with the pinned member shapes; top-level Part ancillaries are not
//     legal inside);
//   - any other member is itself an arm member, so a known arm carrying an
//     unknown extra member is a two-arm part and refused — no provider
//     member can silently disappear through the typed decode.
func validateGeminiPartRaw(raw json.RawMessage, where string) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("gemini: %s part: %w", where, err)
	}
	arms := make([]string, 0, 1)
	for k := range m {
		if !geminiAncillaryPartMembers[k] && !geminiMediaAncillaryMembers[k] {
			arms = append(arms, k)
		}
	}
	if len(arms) != 1 {
		return "", fmt.Errorf("gemini: %s part has %d arm members, want exactly one", where, len(arms))
	}
	arm := arms[0]
	if _, hasThought := m["thought"]; hasThought && arm != "text" {
		return "", fmt.Errorf("gemini: %s part: thought is only legal on a text arm", where)
	}
	if arm != "inlineData" && arm != "fileData" {
		for _, anc := range []string{"videoMetadata", "mediaResolution"} {
			if _, has := m[anc]; has {
				return "", fmt.Errorf("gemini: %s part: %s is only legal on media arms (inlineData/fileData)", where, anc)
			}
		}
	}
	if arm == "functionResponse" {
		if err := validateGeminiFunctionResponse(m["functionResponse"], where); err != nil {
			return "", err
		}
	}
	return arm, nil
}

// validateGeminiFunctionResponse applies the FunctionResponse grammar (REV
// 4 §5): required name + response object; optional id/willContinue;
// scheduling must be the exact wire vocabulary (SCHEDULING_UNSPECIFIED and
// any unknown value -> the value-free 400, absence stays distinct); the
// optional parts array is the SEALED FunctionResponsePart union.
// geminiFunctionResponseMembers is the approved SIX-field inventory of the
// functionResponse object (REV 4 §5): any other member is a value-free 400
// — never a silently dropped provider fact.
var geminiFunctionResponseMembers = map[string]bool{
	"name":         true,
	"response":     true,
	"id":           true,
	"willContinue": true,
	"scheduling":   true,
	"parts":        true,
}

func validateGeminiFunctionResponse(raw json.RawMessage, where string) error {
	var memberSet map[string]json.RawMessage
	if err := json.Unmarshal(raw, &memberSet); err != nil {
		return fmt.Errorf("gemini: %s part functionResponse: %w", where, err)
	}
	for k := range memberSet {
		if !geminiFunctionResponseMembers[k] {
			return fmt.Errorf("gemini: %s part functionResponse: unknown member %q (the approved inventory is name/response/id/willContinue/scheduling/parts)", where, k)
		}
	}
	var fr struct {
		Name         string            `json:"name"`
		Response     json.RawMessage   `json:"response"`
		WillContinue *bool             `json:"willContinue"`
		Scheduling   *string           `json:"scheduling"`
		Parts        []json.RawMessage `json:"parts"`
	}
	if err := json.Unmarshal(raw, &fr); err != nil {
		return fmt.Errorf("gemini: %s part functionResponse: %w", where, err)
	}
	if fr.Name == "" {
		return fmt.Errorf("gemini: %s part functionResponse: name is required", where)
	}
	trimmed := bytes.TrimSpace(fr.Response)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("gemini: %s part functionResponse: response must be a JSON object", where)
	}
	if fr.Scheduling != nil {
		if !geminiSchedulingVocabulary[*fr.Scheduling] {
			return fmt.Errorf("gemini: %s part functionResponse: scheduling %q is not in the vocabulary (SILENT/WHEN_IDLE/INTERRUPT; SCHEDULING_UNSPECIFIED and unknown values are refused)", where, *fr.Scheduling)
		}
	}
	for i, p := range fr.Parts {
		if err := validateGeminiFuncRespPart(p, fmt.Sprintf("%s part functionResponse parts[%d]", where, i)); err != nil {
			return err
		}
	}
	return nil
}

// validateGeminiFuncRespPart applies the sealed FunctionResponsePart
// grammar to the exact one-arm WRAPPER object: exactly one outer arm
// (inlineData|fileData), the arm's INNER bytes validated with the pinned
// member shapes (required members present and typed, displayName
// optional), and NO unknown members at either level. Any other shape is a
// value-free 400.
func validateGeminiFuncRespPart(raw json.RawMessage, where string) error {
	if _, _, err := validateFuncRespPartWrapper(raw); err != nil {
		return fmt.Errorf("gemini: %s: %w", where, err)
	}
	return nil
}

// validateFuncRespPartWrapper is the ONE shared authority for the sealed
// FunctionResponsePart shape, used by BOTH parse (wire -> engine) and
// marshal (engine -> wire): the exact one-arm wrapper object with pinned
// inner member shapes. It returns the arm and the arm's raw inner bytes.
func validateFuncRespPartWrapper(raw json.RawMessage) (string, json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", nil, fmt.Errorf("FunctionResponsePart is not an object")
	}
	var arm string
	var inner json.RawMessage
	for k, v := range m {
		if arm != "" {
			return "", nil, fmt.Errorf("FunctionResponsePart has more than one arm")
		}
		if k != "inlineData" && k != "fileData" {
			return "", nil, fmt.Errorf("FunctionResponsePart has an unknown outer member %q", k)
		}
		arm = k
		inner = v
	}
	if arm == "" {
		return "", nil, fmt.Errorf("FunctionResponsePart has no arm")
	}
	if err := validateFuncRespPartInner(arm, inner); err != nil {
		return "", nil, err
	}
	return arm, inner, nil
}

// validateFuncRespPartInner validates the arm's INNER bytes against the
// pinned member shapes: required members present and correctly typed,
// optional displayName, and no unknown inner members.
func validateFuncRespPartInner(arm string, inner json.RawMessage) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(inner, &m); err != nil {
		return fmt.Errorf("%s is not an object", arm)
	}
	var required, optional []string
	switch arm {
	case "inlineData":
		required = []string{"mimeType", "data"}
		optional = []string{"displayName"}
	case "fileData":
		required = []string{"mimeType", "fileUri"}
		optional = []string{"displayName"}
	}
	for _, k := range required {
		raw, ok := m[k]
		if !ok {
			return fmt.Errorf("%s requires member %q", arm, k)
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return fmt.Errorf("%s member %q must be a string", arm, k)
		}
		if s == "" {
			return fmt.Errorf("%s member %q must be non-empty", arm, k)
		}
	}
	for k := range m {
		known := false
		for _, r := range required {
			if k == r {
				known = true
			}
		}
		for _, o := range optional {
			if k == o {
				known = true
			}
		}
		if !known {
			return fmt.Errorf("%s has an unknown inner member %q", arm, k)
		}
	}
	if raw, ok := m["displayName"]; ok {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return fmt.Errorf("%s member displayName must be a string", arm)
		}
	}
	return nil
}

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

// buildSystemInstruction projects the system messages onto the Gemini
// system array, FAILING CLOSED on any block the system seam cannot
// represent: the provider-independent replacement contract allows any
// block kind in a system message, but Gemini system parts are TEXT-ONLY —
// a silently dropped system Unknown/tool block would be exactly the
// silent-loss class the adapter boundary must reject. Text blocks
// (including explicit empty) survive with their metadata.
func buildSystemInstruction(msgs []engine.Message) (*geminiSystemInstruction, error) {
	var si *geminiSystemInstruction
	for _, msg := range msgs {
		if msg.Role != engine.RoleSystem {
			continue
		}
		if si == nil {
			si = &geminiSystemInstruction{Role: "user"}
		}
		for _, b := range msg.Blocks {
			if b.Text == nil {
				return nil, fmt.Errorf("gemini: system message has a %s block, not representable in the text-only system array", blockKindName(b))
			}
			// Explicit empty text and partMetadata survive.
			si.Parts = append(si.Parts, geminiPart{
				Text: new(b.Text.Text), PartMetadata: b.Text.PartMetadataJson.Bytes(),
			})
		}
	}
	return si, nil
}

// blockKindName names a block for fail-closed diagnostics.
func blockKindName(b engine.Block) string {
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
	}
	return "none"
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
		case engine.RoleSystem:
			// System text is emitted EXCLUSIVELY through
			// systemInstruction (buildSystemInstruction); a duplicate
			// user content would double the system text on the wire.
			continue
		case engine.RoleUser:
			content := geminiContent{Role: "user"}
			for _, b := range msg.Blocks {
				switch {
				case b.Text != nil:
					content.Parts = append(content.Parts, geminiPart{
						Text: new(b.Text.Text), PartMetadata: b.Text.PartMetadataJson.Bytes(),
					})
				case b.Unknown != nil:
					// Unknown part arms are re-emitted RAW at their exact
					// position (the projection invariant is enforced: the
					// payload must not carry a canonical member a plugin
					// could have injected). The typed carriers (signature,
					// partMetadata) are REATTACHED as wire members — a
					// signed media part must round-trip its signature.
					if err := rejectGeminiProjection(b.Unknown); err != nil {
						return nil, err
					}
					payload := b.Unknown.Payload.Bytes()
					if b.Unknown.Signature != "" || len(b.Unknown.PartMetadataJson.Bytes()) > 0 {
						raw, err := geminiPartWithFacts(payload, b.Unknown.Signature, b.Unknown.PartMetadataJson.Bytes())
						if err != nil {
							return nil, err
						}
						content.Parts = append(content.Parts, raw)
					} else {
						content.Parts = append(content.Parts, json.RawMessage(payload))
					}
				case b.ToolResult != nil:
					fr, err := geminiToolResultWire(b.ToolResult, codeAssist)
					if err != nil {
						return nil, err
					}
					content.Parts = append(content.Parts, geminiPart{
						ThoughtSignature: b.ToolResult.Signature,
						PartMetadata:     b.ToolResult.PartMetadataJson.Bytes(),
						FunctionResponse: fr,
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
					textContent.Parts = append(textContent.Parts, geminiPart{
						Text: new(b.Text.Text), ThoughtSignature: b.Text.Signature,
						PartMetadata: b.Text.PartMetadataJson.Bytes(),
					})
				case b.Thinking != nil:
					if textContent == nil {
						textContent = &geminiContent{Role: "model"}
					}
					textContent.Parts = append(textContent.Parts, geminiPart{
						Thought: true, Text: new(b.Thinking.Text), ThoughtSignature: b.Thinking.Signature,
						PartMetadata: b.Thinking.PartMetadataJson.Bytes(),
					})
				case b.Unknown != nil:
					// The SAME projection/reattachment as every other role:
					// a signed media/future Part from the model must not
					// lose thoughtSignature or partMetadata on the wire.
					if err := rejectGeminiProjection(b.Unknown); err != nil {
						return nil, err
					}
					if textContent == nil {
						textContent = &geminiContent{Role: "model"}
					}
					payload := b.Unknown.Payload.Bytes()
					if b.Unknown.Signature != "" || len(b.Unknown.PartMetadataJson.Bytes()) > 0 {
						raw, err := geminiPartWithFacts(payload, b.Unknown.Signature, b.Unknown.PartMetadataJson.Bytes())
						if err != nil {
							return nil, err
						}
						textContent.Parts = append(textContent.Parts, raw)
					} else {
						textContent.Parts = append(textContent.Parts, json.RawMessage(payload))
					}
				case b.ToolUse != nil:
					toolParts = append(toolParts, geminiPart{
						ThoughtSignature: b.ToolUse.Signature,
						PartMetadata:     b.ToolUse.PartMetadataJson.Bytes(),
						FunctionCall:     &geminiFuncCall{Name: b.ToolUse.Name, Args: b.ToolUse.Arguments.Bytes(), ID: b.ToolUse.ID},
					})
				case b.ToolResult != nil:
					fr, err := geminiToolResultWire(b.ToolResult, codeAssist)
					if err != nil {
						return nil, err
					}
					out = append(out, geminiContent{Role: toolResultRole, Parts: []any{geminiPart{
						ThoughtSignature: b.ToolResult.Signature,
						PartMetadata:     b.ToolResult.PartMetadataJson.Bytes(),
						FunctionResponse: fr,
					}}})
				case b.TrailingSignature != nil:
					// The trailing signature-only part is a plain Gemini
					// part (explicit empty text + thoughtSignature), NOT an
					// envelope artifact — it round-trips on the unwrapped
					// shape exactly as unmarshal parsed it.
					if textContent == nil {
						textContent = &geminiContent{Role: "model"}
					}
					textContent.Parts = append(textContent.Parts, geminiPart{
						Text: new(""), ThoughtSignature: b.TrailingSignature.Signature,
						PartMetadata: b.TrailingSignature.PartMetadataJson.Bytes(),
					})
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
				fr, err := geminiToolResultWire(b.ToolResult, codeAssist)
				if err != nil {
					return nil, err
				}
				out = append(out, geminiContent{Role: toolResultRole, Parts: []any{geminiPart{
					ThoughtSignature: b.ToolResult.Signature,
					PartMetadata:     b.ToolResult.PartMetadataJson.Bytes(),
					FunctionResponse: fr,
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
// geminiToolResultWire builds the functionResponse wire object from a
// ToolResult block per the REV 4 §5 SINGLE-AUTHORITY model:
//
//   - the FIRST content element is the response: its Text bytes are the
//     RAW response object — re-emitted VERBATIM when they form a strict
//     JSON object (lossless pass; the text IS the raw, so a content
//     rewrite updates the wire — no stale copy can win);
//   - a non-object text (a plugin rewrite) applies the documented
//     semantic wrap: code assist `{"output": ...}`, bare
//     `{"content": ...}` — the rewrite therefore updates the wire;
//   - subsequent nested Unknown elements become ordered
//     FunctionResponsePart arms (sealed union, preserved payload);
//   - willContinue/scheduling (presence-aware), id/name, and the
//     structured-content grammar are typed; marshal failure is an ERROR,
//     never {}.
func geminiToolResultWire(tr *engine.ToolResultBlock, codeAssist bool) (*geminiFuncResp, error) {
	fr := &geminiFuncResp{Name: tr.ToolName, ID: tr.ToolCallID}
	if tr.WillContinue != nil {
		v := *tr.WillContinue
		fr.WillContinue = &v
	}
	if tr.Scheduling != nil {
		v := *tr.Scheduling
		if !geminiSchedulingVocabulary[v] {
			return nil, fmt.Errorf("gemini: tool result scheduling %q is not in the vocabulary", v)
		}
		fr.Scheduling = &v
	}
	if len(tr.Content) == 0 {
		return nil, fmt.Errorf("gemini: tool result content must be non-empty")
	}
	first := tr.Content[0]
	if first.Unknown != nil || first.CacheBreakpoint != nil {
		return nil, fmt.Errorf("gemini: the FIRST tool-result element must be the response text")
	}
	fr.Response = geminiResponseObject(first.Text, codeAssist)
	for _, c := range tr.Content[1:] {
		if c.Unknown == nil {
			return nil, fmt.Errorf("gemini: only the first element may be text; subsequent elements must be media parts")
		}
		part, err := geminiFuncRespPartFromUnknown(c.Unknown)
		if err != nil {
			return nil, err
		}
		fr.Parts = append(fr.Parts, part)
	}
	return fr, nil
}

// geminiResponseObject is the §5 single-authority response construction:
// text that the SHARED strict object authority accepts is emitted VERBATIM
// (lexeme-exact — numeric lexemes and member order survive, and the same
// validation the platform applies elsewhere rejects duplicate decoded
// keys, escape-equivalent duplicates, lone surrogates, invalid UTF-8, and
// trailing values); any other text gets the documented semantic wrap.
func geminiResponseObject(text string, codeAssist bool) json.RawMessage {
	// The REQUIRED strict-object constructor: empty or whitespace-only
	// text is NOT an absent object (the optional constructor would accept
	// it and emit an empty response) — it gets the semantic wrap.
	if _, err := engine.ParseRequiredJSONObject([]byte(text)); err == nil {
		return json.RawMessage(text) // verbatim, lexeme-exact strict object
	}
	// The documented semantic wrap for a rewritten (non-object) text.
	if codeAssist {
		raw, _ := json.Marshal(map[string]any{"output": text})
		return raw
	}
	raw, _ := json.Marshal(map[string]any{"content": text})
	return raw
}

// geminiFuncRespPartFromUnknown projects a nested Unknown media element
// back to the wire. The payload MUST be the exact one-arm wrapper object —
// the SAME sealed-union validator as parse runs over it, so no
// plugin-added outer/inner member or wrong shape can silently reach the
// provider wire.
func geminiFuncRespPartFromUnknown(u *engine.UnknownBlock) (geminiFuncRespPart, error) {
	arm, inner, err := validateFuncRespPartWrapper(u.Payload.Bytes())
	if err != nil {
		return geminiFuncRespPart{}, fmt.Errorf("gemini: FunctionResponsePart payload: %w", err)
	}
	var out geminiFuncRespPart
	switch arm {
	case "inlineData":
		var b geminiFuncRespBlob
		if err := json.Unmarshal(inner, &b); err != nil {
			return out, fmt.Errorf("gemini: inlineData part: %w", err)
		}
		out.InlineData = &b
	case "fileData":
		var f geminiFuncRespFile
		if err := json.Unmarshal(inner, &f); err != nil {
			return out, fmt.Errorf("gemini: fileData part: %w", err)
		}
		out.FileData = &f
	}
	return out, nil
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

// partMetadata validates a part's raw partMetadata member into the engine
// carrier: ABSENT stays absent (never an invented `{}`); present must be a
// strict JSON object and stays byte-exact (a present `{}` is distinct).
func partMetadata(raw json.RawMessage) (engine.OptionalJSONObject, error) {
	obj, err := engine.ParseOptionalJSONObject(raw)
	if err != nil {
		return engine.OptionalJSONObject{}, fmt.Errorf("gemini: partMetadata: %w", err)
	}
	return obj, nil
}

// geminiRawHasText reports whether the raw part object carries a "text"
// member. A thoughtSignature on a part WITHOUT a text member is a signed
// unknown arm (media/future) — projected with the signature carrier, never
// mistaken for the trailing-standalone shape.
func geminiRawHasText(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m["text"]
	return ok
}

// geminiPartWithFacts splices the typed signature + partMetadata carriers
// back onto a raw unknown part (media/future arms): the raw payload is
// emitted with the facts reattached as wire members. The projection
// invariant guarantees the payload carries no canonical member, so the
// reattachment cannot collide.
func geminiPartWithFacts(payload []byte, signature string, partMetadataJson []byte) (json.RawMessage, error) {
	obj, err := engine.ParseRequiredJSONObject(payload)
	if err != nil {
		return nil, err
	}
	if signature != "" {
		obj, err = obj.SetMember("thoughtSignature", json.RawMessage(fmt.Sprintf("%q", signature)))
		if err != nil {
			return nil, err
		}
	}
	if len(partMetadataJson) > 0 {
		obj, err = obj.SetMember("partMetadata", partMetadataJson)
		if err != nil {
			return nil, err
		}
	}
	return json.RawMessage(obj.Bytes()), nil
}

// stripGenerationCanonicalMembers removes the canonical generationConfig
// members (rebuilt from the PB request) from the preserved inner extras,
// keeping unknown sibling members LOSSLESS: the removal is done with the
// raw wrapper's span operations (DeleteMember on a local object copy,
// then SetMember back), never a map round-trip — member order,
// whitespace, numeric lexemes, escapes, and nested bytes survive.
func stripGenerationCanonicalMembers(inner engine.OptionalJSONObject) (engine.OptionalJSONObject, error) {
	if inner.IsAbsent() {
		return inner, nil
	}
	m, _, err := inner.DecodeObject()
	if err != nil {
		return inner, err
	}
	gcRaw, ok := m["generationConfig"]
	if !ok {
		return inner, nil
	}
	gc, err := engine.ParseOptionalJSONObject(gcRaw)
	if err != nil {
		return inner, fmt.Errorf("gemini code assist: generationConfig must be a strict JSON object: %w", err)
	}
	canonical := []string{"maxOutputTokens", "temperature", "topP", "stopSequences"}
	for _, k := range canonical {
		gc, err = gc.DeleteMember(k)
		if err != nil {
			return inner, err
		}
	}
	if gc.IsAbsent() {
		return inner.DeleteMember("generationConfig")
	}
	return inner.SetMember("generationConfig", gc.Bytes())
}

// readExtRaw returns a raw member of the provider extensions, or nil when
// absent or not an object.
func readExtRaw(ext engine.OptionalJSONObject, key string) []byte {
	if ext.IsAbsent() {
		return nil
	}
	m, _, err := ext.DecodeObject()
	if err != nil {
		return nil
	}
	raw, ok := m[key]
	if !ok {
		return nil
	}
	return raw
}

// VerifyCodeAssistEnvelopePB is the exported candidate validator for the
// plugin seam: it parses the replacement's provider_extensions_json and
// enforces the Code Assist envelope grammar (smuggled canonical members
// refused). The seam runs it AFTER the write-grant verification, per
// plugin, with the settled failure semantics (pass rolls back, block
// refuses); the direct marshal check remains the defensive backstop.
func VerifyCodeAssistEnvelopePB(providerExtensionsJson []byte) error {
	if len(providerExtensionsJson) == 0 {
		// An absent envelope on a code-assist replacement is legal (the
		// marshal defaults it to the empty envelope).
		return nil
	}
	env, err := engine.ParseOptionalJSONObject(providerExtensionsJson)
	if err != nil {
		return fmt.Errorf("code assist envelope: %w", err)
	}
	return verifyCodeAssistEnvelope(env)
}
