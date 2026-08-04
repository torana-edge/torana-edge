package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"

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

// renderInvalidRequest is the host's fail-closed response to a body that a
// KNOWN CONFIGURED format cannot parse (the approved parse-bypass seam): a
// value-free, provider-native HTTP 400 that short-circuits BEFORE rate
// limiting and upstream, identical whether plugins are loaded or not, and
// independent of plugin failure_mode — no request hooks run because there is
// no valid IR to hand them. The message is bounded and names only the
// configured format; it never reflects body or header data (several adapter
// errors embed raw request fragments, so the adapter error itself is never
// surfaced).
func renderInvalidRequest(format string) *BlockResponse {
	code := "invalid_request_error"
	if format == "gemini" || format == "gemini-codeassist" {
		code = "INVALID_ARGUMENT"
	}
	return &BlockResponse{
		Status:      http.StatusBadRequest,
		ContentType: "application/json",
		Body: renderProviderError(format, http.StatusBadRequest, code,
			fmt.Sprintf("the request body could not be parsed as a valid %s request", format)),
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

// renderHostError produces the terminal provider-native 500 for a HOST
// MARSHAL FAILURE: the accepted IR was contract-valid (every replacement
// passed the SDK validator), but the provider adapter cannot project it
// onto the wire. The body is value-free — the diagnostic lives in the
// single sanitized host log line, never echoed to the caller. Served
// synthetically: zero upstream, zero limiter, no response hooks / upstream
// status, no compaction credit.
func renderHostError(format string) *BlockResponse {
	code := "server_error"
	switch format {
	case "anthropic":
		code = "api_error"
	case "gemini", "gemini-codeassist":
		code = "INTERNAL"
	case "bedrock":
		code = "InternalServerException"
	}
	message := "the request could not be encoded for the provider"
	return &BlockResponse{
		Status:      http.StatusInternalServerError,
		ContentType: "application/json",
		Body:        renderProviderError(format, http.StatusInternalServerError, code, message),
	}
}
