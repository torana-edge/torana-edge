package proxy

import (
	"bytes"
	"encoding/json"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
)

// respondVerdict is the plugin-supplied direct response carried in
// ToranaMeta["_respond"] (set via sdk.RespondRequest).
type respondVerdict struct {
	Content string `json:"content"`
}

// renderRespond turns a plugin's raw _respond value into a synthetic 200
// response shaped like the caller's provider — a complete chat completion
// body, or an SSE stream when the client requested streaming. The transport
// returns it verbatim; upstream is never called.
func renderRespond(f *format.Format, model string, raw any, stream bool) *BlockResponse {
	b, _ := json.Marshal(raw)
	var v respondVerdict
	_ = json.Unmarshal(b, &v)

	if stream {
		return &BlockResponse{
			Status:      200,
			ContentType: streamContentType(f.Name),
			Body:        renderCompletionStream(f, v.Content),
		}
	}
	return &BlockResponse{
		Status:      200,
		ContentType: "application/json",
		Body:        renderCompletionJSON(f.Name, model, v.Content),
	}
}

// renderCompletionStream synthesizes a minimal text completion as StreamEvents
// and lets the format's own serializer produce the wire stream — the same code
// path real upstream streams take through the proxy.
func renderCompletionStream(f *format.Format, content string) []byte {
	ch := make(chan engine.StreamEvent, 2)
	ch <- engine.StreamEvent{TextDelta: &content}
	ch <- engine.StreamEvent{FinishReason: "stop"}
	close(ch)
	var buf bytes.Buffer
	_ = f.Stream.SerializeStream(&buf, ch)
	return buf.Bytes()
}

// streamContentType matches what harnesses expect from each provider's
// streaming endpoint. Bedrock streams JSON lines; Gemini/Code Assist and the
// openai family stream SSE (text/event-stream).
func streamContentType(formatName string) string {
	switch formatName {
	case "bedrock":
		return "application/json"
	default:
		return "text/event-stream"
	}
}

// renderCompletionJSON produces a minimal valid non-streaming completion
// envelope per provider format. Usage is reported as zero — no upstream
// tokens were spent.
func renderCompletionJSON(formatName, model, content string) []byte {
	var payload any
	switch formatName {
	case "anthropic":
		payload = map[string]any{
			"id":            "msg_torana_direct",
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []map[string]any{{"type": "text", "text": content}},
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		}
	case "bedrock":
		payload = map[string]any{
			"output": map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": []map[string]any{{"text": content}},
				},
			},
			"stopReason": "end_turn",
			"usage":      map[string]any{"inputTokens": 0, "outputTokens": 0, "totalTokens": 0},
		}
	case "gemini", "gemini-codeassist":
		gen := map[string]any{
			"candidates": []map[string]any{{
				"content": map[string]any{
					"role":  "model",
					"parts": []map[string]any{{"text": content}},
				},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{"promptTokenCount": 0, "candidatesTokenCount": 0, "totalTokenCount": 0},
		}
		// Code Assist nests the GenerateContentResponse under "response".
		if formatName == "gemini-codeassist" {
			payload = map[string]any{"response": gen}
		} else {
			payload = gen
		}
	default: // openai and openai-compatible
		payload = map[string]any{
			"id":     "chatcmpl-torana-direct",
			"object": "chat.completion",
			"model":  model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		}
	}
	out, _ := json.Marshal(payload)
	return out
}
