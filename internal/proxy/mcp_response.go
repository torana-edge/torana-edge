package proxy

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func (s *Server) observeMCPResponse(body []byte, shape string, rs *reqState, status int) {
	if rs == nil || rs.ConversationID == "" || s.mcpCorrelation == nil || status < 200 || status >= 300 || rs.UpstreamStatus < 200 || rs.UpstreamStatus >= 300 {
		return
	}
	cfg := s.GetConfig().Providers.MCP
	if !cfg.Enabled {
		return
	}
	format := shape
	if shape == "openai-chat" || shape == "openai-responses" {
		format = "openai"
	}
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		return
	}
	refs := extractResponse(format, parsed, body)
	response := &engine.ChatResponse{ID: refs.id, Message: refs.assistantMessage(), FinishReason: refs.finishReason, UpstreamStatus: status}
	s.mcpCorrelation.ObserveResponse(response, shape, rs.ConversationID, strconv.FormatUint(rs.ID, 10), cfg.ResponseServerNames(), time.Now())
}
