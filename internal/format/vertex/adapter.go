// Package vertex implements format adapters for GCP Vertex AI / Gemini.
// Gemini uses generateContent / generateContentStream endpoints with a
// content-parts model. Function call arguments come pre-parsed as JSON objects
// (not strings); the stream adapter synthesizes delta events from complete calls.
package vertex

import (
	"encoding/json"
	"fmt"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
)

func init() {
	format.Register("/vertex", format.Format{
		Name:    "vertex",
		Request: &Adapter{},
		Stream:  &StreamAdapter{},
	})
}

// Adapter translates between Gemini generateContent JSON and canonical ChatRequest.
type Adapter struct{}

// --- Wire types for unmarshal/marshal ---

// geminiRequest mirrors the Gemini generateContent request shape.
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
	Parts []geminiPart `json:"parts"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart is a polymorphic content part. Only one of these fields is non-nil.
type geminiPart struct {
	Text             string          `json:"text,omitempty"`
	FunctionCall     *geminiFuncCall `json:"functionCall,omitempty"`
	FunctionResponse *geminiFuncResp `json:"functionResponse,omitempty"`
}

type geminiFuncCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type geminiFuncResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
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

// Unmarshal parses a Gemini generateContent JSON body into a ChatRequest.
func (a *Adapter) Unmarshal(rawBody []byte) (*engine.ChatRequest, error) {
	var gReq geminiRequest
	if err := json.Unmarshal(rawBody, &gReq); err != nil {
		return nil, fmt.Errorf("vertex: unmarshal request: %w", err)
	}

	chat := &engine.ChatRequest{
		Stream: false,
		Model:  "gemini", // Vertex usually has model in URL path
	}

	if gReq.GenerationConfig != nil {
		chat.MaxTokens = gReq.GenerationConfig.MaxOutputTokens
		chat.Temperature = gReq.GenerationConfig.Temperature
		chat.TopP = gReq.GenerationConfig.TopP
		chat.StopSequences = gReq.GenerationConfig.StopSequences
	}

	if len(gReq.SafetySettings) > 0 {
		chat.SafetySettings = gReq.SafetySettings
	}

	var raw map[string]any
	if err := json.Unmarshal(rawBody, &raw); err == nil {
		delete(raw, "systemInstruction")
		delete(raw, "contents")
		delete(raw, "tools")
		delete(raw, "generationConfig")
		delete(raw, "safetySettings")
		if len(raw) > 0 {
			chat.ProviderExtensions = raw
		}
	}

	// System instruction → first Message with RoleSystem.
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
			chat.Messages = append(chat.Messages, engine.Message{
				Role:    engine.RoleSystem,
				Content: sb,
			})
		}
	}

	// Track previous function call indices for tool-result matching.
	// Gemini tool results reference by name, not by call ID.
	// We match to the most recent function call with the same name.
	prevCallIdx := map[string]int{} // toolName → count seen
	callIDs := map[string]string{}  // toolName → most recent generated ID

	for _, content := range gReq.Contents {
		switch content.Role {
		case "user":
			msg := engine.Message{Role: engine.RoleUser}
			hasToolResponses := false
			for _, p := range content.Parts {
				switch {
				case p.Text != "":
					if msg.Content != "" {
						msg.Content += "\n"
					}
					msg.Content += p.Text
				case p.FunctionResponse != nil:
					hasToolResponses = true
					name := p.FunctionResponse.Name
					respJSON, err := json.Marshal(p.FunctionResponse.Response)
					if err != nil {
						respJSON = []byte("{}")
					}
					callID, ok := callIDs[name]
					if !ok {
						callID = name + "_0"
					}
					chat.Messages = append(chat.Messages, engine.Message{
						Role:       engine.RoleTool,
						ToolCallID: callID,
						ToolName:   name,
						Content:    string(respJSON),
					})
				}
			}
			if !hasToolResponses && msg.Content != "" {
				chat.Messages = append(chat.Messages, msg)
			}
		case "model":
			msg := engine.Message{Role: engine.RoleAssistant}
			for _, p := range content.Parts {
				switch {
				case p.Text != "":
					if msg.Content != "" {
						msg.Content += "\n"
					}
					msg.Content += p.Text
				case p.FunctionCall != nil:
					name := p.FunctionCall.Name
					prevCallIdx[name]++
					id := fmt.Sprintf("%s_%d", name, prevCallIdx[name])
					callIDs[name] = id
					msg.ToolCalls = append(msg.ToolCalls, engine.ToolCall{
						ID:        id,
						Name:      name,
						Arguments: p.FunctionCall.Args,
					})
				}
			}
			// Only emit if there's content or tool calls.
			if msg.Content != "" || len(msg.ToolCalls) > 0 {
				chat.Messages = append(chat.Messages, msg)
			}
		default:
			// Unrecognized role — treat as user text.
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

	// Tools: Gemini wraps function declarations inside a tools array.
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

// --- Marshal ---

// Marshal converts a ChatRequest back into Gemini generateContent JSON.
func (a *Adapter) Marshal(chat *engine.ChatRequest) ([]byte, error) {
	gReq := geminiRequest{}

	for _, msg := range chat.Messages {
		switch msg.Role {
		case engine.RoleSystem:
			// System messages → systemInstruction.
			if gReq.SystemInstruction == nil {
				gReq.SystemInstruction = &geminiSystemInstruction{}
			}
			gReq.SystemInstruction.Parts = append(gReq.SystemInstruction.Parts, geminiPart{
				Text: msg.Content,
			})

		case engine.RoleUser:
			content := geminiContent{Role: "user"}
			if msg.Content != "" {
				content.Parts = append(content.Parts, geminiPart{Text: msg.Content})
			}
			// User messages may also carry tool responses if ToolCallID is set.
			if msg.ToolCallID != "" && msg.ToolName != "" {
				var resp map[string]any
				if err := json.Unmarshal([]byte(msg.Content), &resp); err != nil {
					resp = map[string]any{"content": msg.Content}
				}
				content.Parts = append(content.Parts, geminiPart{
					FunctionResponse: &geminiFuncResp{
						Name:     msg.ToolName,
						Response: resp,
					},
				})
			}
			if len(content.Parts) > 0 {
				gReq.Contents = append(gReq.Contents, content)
			}

		case engine.RoleAssistant:
			content := geminiContent{Role: "model"}
			if msg.Content != "" {
				content.Parts = append(content.Parts, geminiPart{Text: msg.Content})
			}
			for _, tc := range msg.ToolCalls {
				content.Parts = append(content.Parts, geminiPart{
					FunctionCall: &geminiFuncCall{
						Name: tc.Name,
						Args: tc.Arguments,
					},
				})
			}
			if len(content.Parts) > 0 {
				gReq.Contents = append(gReq.Contents, content)
			}

		case engine.RoleTool:
			// Tool messages → user content with functionResponse.
			content := geminiContent{Role: "user"}
			var resp map[string]any
			if err := json.Unmarshal([]byte(msg.Content), &resp); err != nil {
				resp = map[string]any{"content": msg.Content}
			}
			content.Parts = append(content.Parts, geminiPart{
				FunctionResponse: &geminiFuncResp{
					Name:     msg.ToolName,
					Response: resp,
				},
			})
			gReq.Contents = append(gReq.Contents, content)
		}
	}

	// Tools: group declarations into the Gemini tool wrapper.
	if len(chat.Tools) > 0 {
		decls := make([]geminiFuncDecl, 0, len(chat.Tools))
		for _, t := range chat.Tools {
			decls = append(decls, geminiFuncDecl{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			})
		}
		gReq.Tools = []geminiTool{{FunctionDeclarations: decls}}
	}

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
