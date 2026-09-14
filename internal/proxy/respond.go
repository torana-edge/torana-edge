package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	"github.com/torana-edge/torana-edge/internal/wasm"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// renderRespond creates a complete host-local response. Identity and accounting
// are host-owned; guest arguments cannot forge an observed provider completion.
func renderRespond(ctx context.Context, f *format.Format, chat *engine.ChatRequest, v *wasm.RespondVerdict) (*BlockResponse, error) {
	if f == nil || chat == nil || v == nil {
		return nil, fmt.Errorf("synthetic response context is missing")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSyntheticForFormat(f, chat, v.Response); err != nil {
		return nil, err
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return nil, err
	}
	id := "torana_" + hex.EncodeToString(entropy[:])
	var body []byte
	var err error
	contentType := "application/json"
	if chat.Stream {
		contentType = "text/event-stream"
		body, err = renderSyntheticStream(ctx, f, chat, v.Response, id)
	} else {
		body, err = renderSyntheticJSON(f.Name, chat, v.Response, id)
	}
	if err != nil {
		return nil, err
	}
	return &BlockResponse{Status: 200, ContentType: contentType, Body: body}, nil
}

func renderSyntheticStream(ctx context.Context, f *format.Format, chat *engine.ChatRequest, response *pb.SyntheticResponse, id string) ([]byte, error) {
	if f.Stream == nil {
		return nil, fmt.Errorf("format has no streaming response renderer")
	}
	if err := validateSyntheticBlocks(response); err != nil {
		return nil, err
	}
	events := make(chan engine.StreamEvent, 3*len(response.Message.Blocks)+3)
	events <- engine.StreamEvent{MessageStart: &engine.StreamMessageStart{Role: "assistant", ID: id, Model: chat.Model}}
	for index, block := range response.Message.Blocks {
		if text := block.GetText(); text != nil {
			events <- engine.StreamEvent{BlockStart: &engine.BlockStart{Index: index, Kind: engine.BlockKindText}}
			value := text.Text
			events <- engine.StreamEvent{TextDelta: &value}
			events <- engine.StreamEvent{BlockStop: &engine.BlockStop{Index: index}}
		} else if tool := block.GetToolCall(); tool != nil {
			events <- engine.StreamEvent{ToolCallStart: &engine.ToolCallStart{Index: index, ID: fmt.Sprintf("%s_call_%d", id, index), Name: tool.Name, InvocationKind: engine.ToolInvocationFunction}}
			events <- engine.StreamEvent{ToolCallDelta: &engine.ToolCallDelta{Index: index, ArgumentsDelta: string(tool.ArgumentsJson)}}
			events <- engine.StreamEvent{ToolCallEnd: &engine.ToolCallEnd{Index: index}}
		}
	}
	events <- engine.StreamEvent{Usage: &engine.StreamUsage{}}
	events <- engine.StreamEvent{FinishReason: response.FinishReason}
	close(events)
	var out bytes.Buffer
	ctx = context.WithValue(ctx, engine.ChatRequestKey, chat)
	if err := f.Stream.SerializeStream(ctx, &out, events); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func renderSyntheticJSON(provider string, chat *engine.ChatRequest, response *pb.SyntheticResponse, id string) ([]byte, error) {
	if err := validateSyntheticBlocks(response); err != nil {
		return nil, err
	}
	blocks := response.Message.Blocks
	var payload any
	switch provider {
	case "anthropic":
		content := make([]any, 0, len(blocks))
		for i, b := range blocks {
			if text := b.GetText(); text != nil {
				content = append(content, map[string]any{"type": "text", "text": text.Text})
			} else {
				tool := b.GetToolCall()
				content = append(content, map[string]any{"type": "tool_use", "id": fmt.Sprintf("%s_call_%d", id, i), "name": tool.Name, "input": json.RawMessage(tool.ArgumentsJson)})
			}
		}
		reason := "end_turn"
		if response.FinishReason == "tool_calls" {
			reason = "tool_use"
		}
		payload = map[string]any{"id": id, "type": "message", "role": "assistant", "model": chat.Model, "content": content, "stop_reason": reason, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}
	case "gemini", "gemini-codeassist":
		parts := make([]any, 0, len(blocks))
		for i, b := range blocks {
			if text := b.GetText(); text != nil {
				parts = append(parts, map[string]any{"text": text.Text})
			} else {
				tool := b.GetToolCall()
				parts = append(parts, map[string]any{"functionCall": map[string]any{"id": fmt.Sprintf("%s_call_%d", id, i), "name": tool.Name, "args": json.RawMessage(tool.ArgumentsJson)}})
			}
		}
		generated := map[string]any{"responseId": id, "modelVersion": chat.Model, "candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": parts}, "finishReason": "STOP"}}, "usageMetadata": map[string]int{"promptTokenCount": 0, "candidatesTokenCount": 0, "totalTokenCount": 0}}
		payload = generated
		if provider == "gemini-codeassist" || chat.CodeAssist {
			payload = map[string]any{"response": generated}
		}
	case "openai":
		if chat.OpenAIVariant == engine.OpenAIResponses {
			output := make([]any, 0, len(blocks))
			for i, b := range blocks {
				if text := b.GetText(); text != nil {
					output = append(output, map[string]any{"id": fmt.Sprintf("%s_msg_%d", id, i), "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text.Text, "annotations": []any{}}}})
				} else {
					tool := b.GetToolCall()
					output = append(output, map[string]any{"id": fmt.Sprintf("%s_item_%d", id, i), "type": "function_call", "status": "completed", "call_id": fmt.Sprintf("%s_call_%d", id, i), "name": tool.Name, "arguments": string(tool.ArgumentsJson)})
				}
			}
			payload = map[string]any{"id": id, "object": "response", "model": chat.Model, "status": "completed", "output": output, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}
		} else {
			var text strings.Builder
			tools := []any{}
			seenTool := false
			for i, b := range blocks {
				if content := b.GetText(); content != nil {
					if seenTool {
						return nil, fmt.Errorf("chat completions cannot represent text after a tool-call block")
					}
					text.WriteString(content.Text)
				} else {
					seenTool = true
					tool := b.GetToolCall()
					tools = append(tools, map[string]any{"id": fmt.Sprintf("%s_call_%d", id, i), "type": "function", "function": map[string]any{"name": tool.Name, "arguments": string(tool.ArgumentsJson)}})
				}
			}
			message := map[string]any{"role": "assistant", "content": text.String()}
			if len(tools) > 0 {
				message["tool_calls"] = tools
			}
			payload = map[string]any{"id": id, "object": "chat.completion", "model": chat.Model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": response.FinishReason}}, "usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}}
		}
	default:
		return nil, fmt.Errorf("format has no synthetic response renderer")
	}
	return json.Marshal(payload)
}

func validateSyntheticBlocks(response *pb.SyntheticResponse) error {
	if response == nil || response.Message == nil {
		return fmt.Errorf("synthetic response message is missing")
	}
	for i, block := range response.Message.Blocks {
		if block == nil || (block.GetText() == nil && block.GetToolCall() == nil) {
			return fmt.Errorf("synthetic response block %d has no supported arm", i)
		}
	}
	return nil
}

type syntheticResponseScopeKey struct{}
type syntheticResponseScope struct {
	format  *format.Format
	request *engine.ChatRequest
}

func validateSyntheticForFormat(f *format.Format, chat *engine.ChatRequest, response *pb.SyntheticResponse) error {
	if err := response.Validate(); err != nil {
		return err
	}
	if f == nil || chat == nil {
		return fmt.Errorf("synthetic response format context is missing")
	}
	switch f.Name {
	case "openai", "anthropic", "gemini", "gemini-codeassist":
	default:
		return fmt.Errorf("format has no synthetic response renderer")
	}
	if chat.Stream && f.Stream == nil {
		return fmt.Errorf("format has no streaming response renderer")
	}
	if f.Name == "openai" && chat.OpenAIVariant != engine.OpenAIResponses {
		seenTool := false
		for _, block := range response.Message.Blocks {
			if block.GetToolCall() != nil {
				seenTool = true
			} else if seenTool {
				return fmt.Errorf("chat completions cannot represent text after a tool-call block")
			}
		}
	}
	return nil
}
