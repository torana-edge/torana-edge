package proxy

import (
	"net/http"

	"github.com/torana-edge/torana-edge/internal/bridge"
)

// Local failures use the ingress contract even before an inference request has
// been parsed. Their attribution remains local: they have no upstream status.
func bridgeLocalError(client bridge.Protocol, status int, message string) *BlockResponse {
	code := "invalid_request_error"
	if status == http.StatusTooManyRequests {
		code = "rate_limit_error"
	}
	if client == bridge.Gemini || client == bridge.GeminiCodeAssist {
		code = "INVALID_ARGUMENT"
		if status == http.StatusTooManyRequests {
			code = "RESOURCE_EXHAUSTED"
		}
	}
	return &BlockResponse{Status: status, ContentType: "application/json", Body: renderProviderError(client.Format(), status, code, message)}
}

func writeBridgeLocalError(w http.ResponseWriter, client bridge.Protocol, status int, message string) {
	result := bridgeLocalError(client, status, message)
	w.Header().Set("Content-Type", result.ContentType)
	w.WriteHeader(result.Status)
	_, _ = w.Write(result.Body)
}

// Representation validators belong to the upstream bytes, not a translated
// response. Retaining them would let caches validate a body they never saw.
func clearBridgeRepresentationHeaders(h http.Header) {
	for _, name := range []string{"Content-Encoding", "Content-Length", "ETag", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Content-Range", "Accept-Ranges"} {
		h.Del(name)
	}
}
