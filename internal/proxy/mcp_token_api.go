package proxy

import (
	"encoding/json"
	"io"
	"net/http"
)

const mcpTokenAPIPath = "/_torana/api/v1/mcp/token"

// Tokens are operator-only credentials, never namespace operations. Both
// retrieval/setup and rotation are POSTs behind the local mutation guard:
// cross-origin browser GETs must not be able to read or provision a token.
func (s *Server) handleMCPToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST for MCP token setup or rotation")
		return
	}
	var input map[string]json.RawMessage
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	var trailing any
	if decoder.Decode(&input) != nil || input == nil || len(input) != 0 || decoder.Decode(&trailing) != io.EOF {
		writeAgentError(w, http.StatusBadRequest, "invalid_input", "send an empty JSON object")
		return
	}
	var token string
	var err error
	switch r.URL.Path {
	case mcpTokenAPIPath + "/rotate":
		token, err = s.mcpTokens.Rotate()
	case mcpTokenAPIPath + "/setup":
		token, err = s.mcpTokens.Ensure()
	case mcpTokenAPIPath:
		token, err = s.mcpTokens.Current()
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		// Storage/cryptography errors are not safe response text.
		writeAgentError(w, http.StatusServiceUnavailable, "token_unavailable", "MCP token storage is unavailable; retry after checking the instance")
		return
	}
	if token == "" {
		writeAgentError(w, http.StatusConflict, "token_not_configured", "set up MCP with torana mcp enable --yes")
		return
	}
	writeAgentJSON(w, http.StatusOK, struct {
		Token string `json:"token"`
	}{Token: token})
}
