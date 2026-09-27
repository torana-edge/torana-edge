package proxy

import (
	"context"
	"log"
	"time"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/plugin"
)

const maxTranscriptOperations = 16

// observeTranscriptOperations supplies the conversation binding that an
// out-of-band MCP call may not have had. It can only create a user confirmation
// request; it never applies a change and never re-executes reads.
func (s *Server) observeTranscriptOperations(ctx context.Context, request *engine.ChatRequest, conversation string, servers []string) {
	if conversation == "" || s.suggestions == nil {
		return
	}
	calls := mcpserver.TranscriptInvocations(request, servers)
	if len(calls) == 0 {
		return
	}
	policy, err := s.mcpPolicy()
	if err != nil {
		log.Printf("[mcp] could not resolve transcript operation policy: %v", err)
		return
	}
	releaseConversation, allowed := s.mcpLimits.acquireLease("conversation:" + conversation)
	if !allowed {
		return
	}
	defer releaseConversation()
	dispatch := operationDispatch{policy: policy, execute: s.executeNamespaceOperation, propose: s.proposeNamespaceOperation}
	if len(calls) > maxTranscriptOperations {
		calls = calls[:maxTranscriptOperations]
	}
	for _, call := range calls {
		releaseTool, allowed := s.mcpLimits.acquireLease("tool:torana_invoke")
		if !allowed {
			return
		}
		start := time.Now()
		if err := s.consumeTranscriptTicket(ctx, call.Ticket, call.Input); err != nil {
			releaseTool()
			continue
		}
		result, handled, callErr := dispatch.invokeTranscript(ctx, call.Input, plugin.MCPBinding{Bound: true, ConversationID: conversation, CallID: call.CallID})
		releaseTool()
		if !handled {
			continue
		}
		s.appendMCPAudit("torana_invoke", true, result, callErr, start)
		if callErr != nil {
			log.Printf("[mcp] could not bind transcript operation: %v", callErr)
		}
	}
}
