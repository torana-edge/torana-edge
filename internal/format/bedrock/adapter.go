// Package bedrock implements format adapters for AWS Bedrock Converse API.
// Bedrock uses content blocks (text, toolUse, toolResult) similar to Anthropic
// but nests tool definitions under toolConfig.tools[].toolSpec and parameters
// under inputSchema.json.
package bedrock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

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
	ModelID     string                 `json:"modelId"`
	System      json.RawMessage        `json:"system"`
	Messages    []bedrockMsg           `json:"messages"`
	ToolConfig  *bedrockToolConfig     `json:"toolConfig,omitempty"`
	InferenceConfig *bedrockInferenceConfig `json:"inferenceConfig,omitempty"`
}

type bedrockMsg struct {
	Role    string             `json:"role"`
	Content json.RawMessage    `json:"content"` // array of content blocks
}

type bedrockContentBlock struct {
	Text       *string              `json:"text,omitempty"`
	Thinking   *bedrockThinking     `json:"thinking,omitempty"`
	ToolUse    *bedrockToolUse      `json:"toolUse,omitempty"`
	ToolResult *bedrockToolResult   `json:"toolResult,omitempty"`
}

type bedrockThinking struct {
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type bedrockToolUse struct {
	ToolUseID string         `json:"toolUseId"`
	Name      string         `json:"name"`
	Input     map[string]any `json:"input"`
}

type bedrockToolResult struct {
	ToolUseID string                `json:"toolUseId"`
	Content   []bedrockContentBlock `json:"content"`
}

type bedrockToolConfig struct {
	Tools []bedrockTool `json:"tools"`
}

type bedrockTool struct {
	ToolSpec bedrockToolSpec `json:"toolSpec"`
}

type bedrockToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema bedrockSchema  `json:"inputSchema"`
}

type bedrockSchema struct {
	JSON map[string]any `json:"json"`
}

type bedrockSystemBlock struct {
	Text string `json:"text"`
}

type bedrockInferenceConfig struct {
	MaxTokens     *int     `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

// --- Unmarshal ---

func (a *Adapter) Unmarshal(httpReq *http.Request, rawBody []byte) (*engine.ChatRequest, error) {
	if len(rawBody) == 0 {
		return nil, fmt.Errorf("bedrock: empty request body")
	}

	var req bedrockRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		return nil, fmt.Errorf("bedrock: unmarshal request: %w", err)
	}

	chat := &engine.ChatRequest{Model: req.ModelID}

	// Parse system blocks
	if len(req.System) > 0 {
		sysBlocks, err := parseSystemBlocks(req.System)
		if err != nil {
			return nil, fmt.Errorf("bedrock: unmarshal system: %w", err)
		}
		if len(sysBlocks) > 0 {
			chat.Messages = append(chat.Messages, engine.Message{
				Role:    engine.RoleSystem,
				Content: sysBlocks,
			})
		}
	}

	// Parse messages
	for _, bm := range req.Messages {
		blocks, err := parseContentBlocks(bm.Content)
		if err != nil {
			return nil, fmt.Errorf("bedrock: unmarshal message content: %w", err)
		}

		msgs := blocksToMessages(bm.Role, blocks)
		chat.Messages = append(chat.Messages, msgs...)
	}

	// Parse tools
	if req.ToolConfig != nil {
		for _, t := range req.ToolConfig.Tools {
			td := engine.ToolDef{
				Name:        t.ToolSpec.Name,
				Description: t.ToolSpec.Description,
				Parameters:  t.ToolSpec.InputSchema.JSON,
			}
			chat.Tools = append(chat.Tools, td)
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

	var raw map[string]any
	if err := json.Unmarshal(rawBody, &raw); err == nil {
		delete(raw, "modelId")
		delete(raw, "system")
		delete(raw, "messages")
		delete(raw, "toolConfig")
		delete(raw, "inferenceConfig")
		if len(raw) > 0 {
			chat.ProviderExtensions = raw
		}
	}

	return chat, nil
}

// parseSystemBlocks parses the system array into a concatenated string.
func parseSystemBlocks(raw json.RawMessage) (string, error) {
	var blocks []bedrockSystemBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}
	var parts []string
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n"), nil
}

// parseContentBlocks unmarshals the content JSON as an array of bedrockContentBlock.
func parseContentBlocks(raw json.RawMessage) ([]bedrockContentBlock, error) {
	var blocks []bedrockContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

// blocksToMessages converts content blocks into one or more engine.Message values.
// Text blocks are concatenated into a single message. Tool use and tool result blocks
// each produce their own message.
func blocksToMessages(role string, blocks []bedrockContentBlock) []engine.Message {
	var msgs []engine.Message
	var textParts []string
	var thinkingText string
	var thinkingSig string

	irRole := mapRole(role)

	flushText := func() {
		if len(textParts) > 0 || thinkingText != "" {
			msgs = append(msgs, engine.Message{
				Role:              irRole,
				Content:           strings.Join(textParts, ""),
				Thinking:          thinkingText,
				ThinkingSignature: thinkingSig,
			})
			textParts = nil
			thinkingText = ""
			thinkingSig = ""
		}
	}

	for _, b := range blocks {
		switch {
		case b.Text != nil:
			textParts = append(textParts, *b.Text)
		case b.Thinking != nil:
			thinkingText = b.Thinking.Thinking
			thinkingSig = b.Thinking.Signature
		case b.ToolUse != nil:
			// Flush pending text first
			flushText()
			msgs = append(msgs, engine.Message{
				Role: engine.RoleAssistant,
				ToolCalls: []engine.ToolCall{{
					ID:        b.ToolUse.ToolUseID,
					Name:      b.ToolUse.Name,
					Arguments: b.ToolUse.Input,
				}},
			})
		case b.ToolResult != nil:
			// Flush pending text first
			flushText()
			resultContent := ""
			if len(b.ToolResult.Content) > 0 && b.ToolResult.Content[0].Text != nil {
				resultContent = *b.ToolResult.Content[0].Text
			}
			msgs = append(msgs, engine.Message{
				Role:       engine.RoleTool,
				ToolCallID: b.ToolResult.ToolUseID,
				Content:    resultContent,
			})
		}
	}

	// Flush any remaining text
	flushText()

	return msgs
}

// mapRole converts Bedrock role strings to engine.Role.
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

// --- Marshal ---
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
		if m.Role == engine.RoleSystem && m.Content != "" {
			req.System = marshalSystemBlocks(m.Content)
			break // only first system message
		}
	}

	// Messages (excluding system)
	for _, m := range chat.Messages {
		if m.Role == engine.RoleSystem {
			continue
		}
		bm := marshalMessage(m)
		req.Messages = append(req.Messages, bm)
	}

	// Tools
	if len(chat.Tools) > 0 {
		req.ToolConfig = &bedrockToolConfig{}
		for _, td := range chat.Tools {
			req.ToolConfig.Tools = append(req.ToolConfig.Tools, bedrockTool{
				ToolSpec: bedrockToolSpec{
					Name:        td.Name,
					Description: td.Description,
					InputSchema: bedrockSchema{
						JSON: td.Parameters,
					},
				},
			})
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

func marshalSystemBlocks(text string) json.RawMessage {
	blocks := []bedrockSystemBlock{{Text: text}}
	b, _ := json.Marshal(blocks)
	return b
}

func marshalMessage(m engine.Message) bedrockMsg {
	bm := bedrockMsg{
		Role: reverseRole(m.Role),
	}

	var blocks []bedrockContentBlock

	switch m.Role {
	case engine.RoleTool:
		// Tool result: only emit toolResult block, not a text block.
		content := m.Content
		blocks = append(blocks, bedrockContentBlock{
			ToolResult: &bedrockToolResult{
				ToolUseID: m.ToolCallID,
				Content: []bedrockContentBlock{{
					Text: &content,
				}},
			},
		})

	case engine.RoleAssistant:
		// Assistant may have thinking, text, tool calls, or combinations.
		if m.Thinking != "" {
			blocks = append(blocks, bedrockContentBlock{
				Thinking: &bedrockThinking{
					Thinking:  m.Thinking,
					Signature: m.ThinkingSignature,
				},
			})
		}
		if m.Content != "" {
			text := m.Content
			blocks = append(blocks, bedrockContentBlock{Text: &text})
		}
		for _, tc := range m.ToolCalls {
			blocks = append(blocks, bedrockContentBlock{
				ToolUse: &bedrockToolUse{
					ToolUseID: tc.ID,
					Name:      tc.Name,
					Input:     tc.Arguments,
				},
			})
		}

	default:
		// User (or any other role): just text.
		if m.Content != "" {
			text := m.Content
			blocks = append(blocks, bedrockContentBlock{Text: &text})
		}
	}

	raw, _ := json.Marshal(blocks)
	bm.Content = raw
	return bm
}

// reverseRole converts engine.Role to Bedrock role strings.
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
