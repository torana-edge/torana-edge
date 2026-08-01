package proxy

import (
	"encoding/json"

	"github.com/torana-edge/torana-edge/internal/wasm"
)

// renderBlock turns a recorded block verdict into a synthetic response,
// rendering the error body in the caller's provider format.
//
// The verdict is a typed value now. v1 round-tripped it through
// ToranaMeta["_block"] as an `any`, so it was re-marshalled and re-parsed to
// find out what the plugin had said — and a malformed value silently became
// the defaults.
func renderBlock(format string, v *wasm.BlockVerdict) *BlockResponse {
	status := int(v.Status)
	if status == 0 {
		status = 422
	}
	code := v.Code
	if code == "" {
		code = "request_blocked"
	}
	return &BlockResponse{
		Status:      status,
		ContentType: "application/json",
		Body:        renderProviderError(format, status, code, v.Message),
	}
}

// renderProviderError produces an error body shaped like the caller's provider
// so the agent harness parses it the same as any upstream API error.
func renderProviderError(format string, status int, code, message string) []byte {
	var payload any
	switch format {
	case "anthropic":
		payload = map[string]any{
			"type":  "error",
			"error": map[string]any{"type": code, "message": message},
		}
	case "gemini", "gemini-codeassist":
		// Google API errors are a bare {error:{…}} even on Code Assist streams.
		payload = map[string]any{
			"error": map[string]any{"code": status, "status": code, "message": message},
		}
	case "bedrock":
		payload = map[string]any{"message": message}
	default: // openai and openai-compatible
		payload = map[string]any{
			"error": map[string]any{"message": message, "type": code, "code": code},
		}
	}
	out, _ := json.Marshal(payload)
	return out
}
