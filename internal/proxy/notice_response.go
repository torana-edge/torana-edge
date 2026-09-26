package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func noticeShape(clientFormat string, chat *engine.ChatRequest) string {
	if clientFormat == "openai" {
		if isOpenAIResponsesRequest(chat) {
			return "openai-responses"
		}
		return "openai-chat"
	}
	return clientFormat
}

// appendNoticeJSON changes a completed, tool-free response in the CLIENT'S
// wire shape. An incomplete or tool-calling turn is returned byte-for-byte.
// Streaming has a separate event-level path; this function handles only JSON.
func appendNoticeJSON(body []byte, shape, notice, id string) ([]byte, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, false, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, false, fmt.Errorf("trailing response JSON")
	}
	if root == nil {
		return body, false, nil
	}
	switch shape {
	case "anthropic":
		if root["stop_reason"] != "end_turn" && root["stop_reason"] != "stop_sequence" {
			return body, false, nil
		}
		for _, block := range array(root["content"]) {
			if object(block)["type"] == "tool_use" {
				return body, false, nil
			}
		}
		part, _ := json.Marshal(map[string]any{"type": "text", "text": notice})
		return appendJSONElement(body, part, "content")
	case "openai-chat":
		choices := array(root["choices"])
		if len(choices) == 0 || object(choices[0])["finish_reason"] != "stop" {
			return body, false, nil
		}
		message := object(object(choices[0])["message"])
		if len(array(message["tool_calls"])) > 0 {
			return body, false, nil
		}
		content, ok := message["content"].(string)
		if !ok {
			return body, false, nil
		}
		start, end, found := rawJSONSpanAt(body, "choices", 0, "message", "content")
		if !found {
			return nil, false, fmt.Errorf("OpenAI response content span missing")
		}
		value, _ := json.Marshal(content + notice)
		return spliceBytes(body, start, end, value), true, nil
	case "openai-responses":
		if root["status"] != "completed" {
			return body, false, nil
		}
		for _, item := range array(root["output"]) {
			if kind := object(item)["type"]; kind == "function_call" || kind == "custom_tool_call" {
				return body, false, nil
			}
		}
		item, _ := json.Marshal(map[string]any{
			"id": "torana_notice_" + id, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": notice, "annotations": []any{}}},
		})
		return appendJSONElement(body, item, "output")
	case "gemini", "gemini-codeassist":
		prefix := []any{}
		current := root
		if shape == "gemini-codeassist" {
			current = object(root["response"])
			prefix = []any{"response"}
		}
		candidates := array(current["candidates"])
		if len(candidates) == 0 || object(candidates[0])["finishReason"] != "STOP" {
			return body, false, nil
		}
		parts := array(object(object(candidates[0])["content"])["parts"])
		for _, part := range parts {
			if object(part)["functionCall"] != nil {
				return body, false, nil
			}
		}
		part, _ := json.Marshal(map[string]string{"text": notice})
		path := append(prefix, "candidates", 0, "content", "parts")
		return appendJSONElement(body, part, path...)
	}
	return body, false, nil
}

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func array(value any) []any {
	result, _ := value.([]any)
	return result
}

func appendJSONElement(body, element []byte, path ...any) ([]byte, bool, error) {
	start, end, ok := rawJSONSpanAt(body, path...)
	if !ok || start >= end || body[start] != '[' || body[end-1] != ']' {
		return nil, false, fmt.Errorf("notice target array is missing")
	}
	comma := []byte{}
	if len(bytes.TrimSpace(body[start+1:end-1])) > 0 {
		comma = []byte{','}
	}
	insert := append(comma, element...)
	return spliceBytes(body, end-1, end-1, insert), true, nil
}
