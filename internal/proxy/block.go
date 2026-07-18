package proxy

import "encoding/json"

// blockVerdict is the plugin-supplied rejection carried in ToranaMeta["_block"]
// (set via sdk.BlockRequest). Status/code default if a plugin omits them.
type blockVerdict struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// renderBlock turns a plugin's raw _block value into a synthetic response,
// rendering the error body in the caller's provider format.
func renderBlock(format string, raw any) *BlockResponse {
	b, _ := json.Marshal(raw)
	var v blockVerdict
	_ = json.Unmarshal(b, &v)
	if v.Status == 0 {
		v.Status = 422
	}
	if v.Code == "" {
		v.Code = "request_blocked"
	}
	return &BlockResponse{
		Status:      v.Status,
		ContentType: "application/json",
		Body:        renderProviderError(format, v.Status, v.Code, v.Message),
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
