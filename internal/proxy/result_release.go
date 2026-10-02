package proxy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"github.com/torana-edge/torana-edge/internal/resultrelease"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

const resultReleaseAPIPath = "/_torana/api/v1/approvals"

var releaseRequestSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["reference"],"properties":{"reference":{"type":"string","description":"Opaque tr_ reference from the PII tool error"}}}`)

func resultReleaseAgentOperations() []agentAPIOperation {
	operations := []agentAPIOperation{
		{ID: "torana.approvals.list", Method: http.MethodGet, Path: resultReleaseAPIPath, Description: "List up to 100 withheld result records. Metadata only; pass next_cursor as the cursor query parameter for another page.", Risk: "read", Idempotent: true, ContentType: "application/json", OutputSchema: arbitraryObjectSchema},
		{ID: "torana.approvals.show", Method: http.MethodGet, Path: resultReleaseAPIPath + "/{reference}", Description: "Read the current status and exact scope of one withheld result, without original content.", Risk: "read", Idempotent: true, ContentType: "application/json", OutputSchema: arbitraryObjectSchema},
	}
	for _, action := range []string{"approve", "decline", "revoke"} {
		operations = append(operations, agentAPIOperation{ID: "torana.approvals." + action, Method: http.MethodPost, Path: resultReleaseAPIPath + "/{reference}/" + action, Description: "Operator-only decision for one exact result. Supply its reviewed expected_status; conflicts do not apply. Approval allows original content upstream, not a clean scan verdict.", Risk: "write", Idempotent: true, ContentType: "application/json", InputSchema: json.RawMessage(`{"type":"object","required":["expected_status"],"properties":{"expected_status":{"type":"string"}},"additionalProperties":false}`), OutputSchema: arbitraryObjectSchema})
	}
	return operations
}

func (s *Server) observeToolResultRelease(ctx context.Context, pluginName, digest string, result *pb.RequestToolResultBlock, register bool) ([]byte, *pb.HostError) {
	refusal := func(code pb.ErrorCode, message string) ([]byte, *pb.HostError) {
		return nil, &pb.HostError{Code: code, Message: message}
	}
	state := reqStateFrom(ctx)
	if state == nil || state.ConversationID == "" || result == nil {
		return refusal(pb.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "stable result identity is unavailable")
	}
	// Bare Gemini calls have semantic synthetic IDs plus a transcript ordinal.
	// Compaction can reuse that ordinal for another identical call. A content
	// verdict can be shared, but a human exception must not silently carry over.
	if result.ToolCallId == "" || strings.HasPrefix(result.ToolCallId, "torana_gemini_") {
		return []byte(`{"reference":"","approved":false}`), nil
	}
	// Cache carriers can move during compaction without changing tool content.
	var content []*pb.ToolResultContentBlock
	for _, arm := range result.Content {
		if arm == nil || arm.GetCacheBreakpoint() == nil {
			content = append(content, arm)
		}
	}
	fingerprint, err := sdk.ToolResultContentFingerprint(content)
	if err != nil {
		return refusal(pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "unrepresentable tool content")
	}
	item, found, err := s.resultReleases.Observe(resultrelease.Scope{Conversation: state.ConversationID, Plugin: pluginName, Digest: digest, CallID: result.ToolCallId, ContentHash: hex.EncodeToString(fingerprint[:])}, register)
	if err != nil {
		return refusal(pb.ErrorCode_ERROR_CODE_UNAVAILABLE, "result approval state is unavailable")
	}
	info := sdk.ToolResultReleaseInfo{}
	if found {
		info.Reference, info.Approved = item.Reference, item.Status == "approved"
	}
	raw, _ := json.Marshal(info)
	return raw, nil
}

// Model-facing: requesting review is not approval, and no request payload can
// select a different conversation. A declined/revoked decision stays declined.
func (s *Server) requestToolResultRelease(call operationCall) (mcpserver.Result, error) {
	var input struct {
		Reference string `json:"reference"`
	}
	if json.Unmarshal(call.Input, &input) != nil || !resultrelease.ValidReference(input.Reference) {
		return operationError("invalid_input", "Use the withheld result's reference."), nil
	}
	if !call.Binding.Bound || call.Binding.ConversationID == "" {
		return operationError("unbound_conversation", "Retry after Torana observes this tool call."), nil
	}
	item, err := s.resultReleases.Request(input.Reference, call.Binding.ConversationID)
	if errors.Is(err, resultrelease.ErrNotFound) {
		return operationError("not_found", "That result is not available in this conversation."), nil
	}
	if err != nil {
		return operationError("state_unavailable", "Torana could not record this request. Try again."), nil
	}
	status := item.Status
	summary := "Ask the user to review this result in Torana's Approvals page or CLI. Do not retry the read to bypass the scan."
	if status == "approved" {
		summary = "The user allowed this exact result. Torana can forward it when the harness resends it; another tool call needs its own approval."
	}
	return mcpserver.Result{OK: true, Status: status, Summary: summary, Conversation: &mcpserver.ConversationBinding{Binding: "bound"}}, nil
}

func (s *Server) handleResultApprovals(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, resultReleaseAPIPath)
	if path == "" || path == "/" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeAgentError(w, 405, "method_not_allowed", "List approvals with GET.")
			return
		}
		items, next, err := s.resultReleases.List(r.URL.Query().Get("cursor"))
		if err != nil {
			writeAgentError(w, 503, "state_unavailable", "Could not load approvals; try again.")
			return
		}
		writeAgentJSON(w, 200, map[string]any{"approvals": items, "next_cursor": next})
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) == 1 && resultrelease.ValidReference(parts[0]) && r.Method == http.MethodGet {
		item, err := s.resultReleases.Get(parts[0])
		if errors.Is(err, resultrelease.ErrNotFound) {
			writeAgentError(w, 404, "not_found", "Approval not found.")
			return
		}
		if err != nil {
			writeAgentError(w, 503, "state_unavailable", "Could not read approval state.")
			return
		}
		writeAgentJSON(w, 200, item)
		return
	}
	if len(parts) != 2 || !resultrelease.ValidReference(parts[0]) || (parts[1] != "approve" && parts[1] != "decline" && parts[1] != "revoke") {
		writeAgentError(w, 404, "not_found", "Approval not found.")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAgentError(w, 405, "method_not_allowed", "Change approvals with POST.")
		return
	}
	var input struct {
		Status string `json:"expected_status"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF || input.Status == "" {
		writeAgentError(w, 400, "invalid_input", "Supply the reviewed expected_status.")
		return
	}
	// Serialise with bundle changes. The UI/CLI cannot allow a stale bundle.
	s.controlPlaneMutationMu.Lock()
	defer s.controlPlaneMutationMu.Unlock()
	item, err := s.resultReleases.Get(parts[0])
	if errors.Is(err, resultrelease.ErrNotFound) {
		writeAgentError(w, 404, "not_found", "Approval not found.")
		return
	}
	if err != nil {
		writeAgentError(w, 503, "state_unavailable", "Could not read approval state.")
		return
	}
	if parts[1] == "approve" {
		registry, err := s.currentNamespaceRegistryLocked()
		if err != nil {
			writeAgentError(w, 503, "state_unavailable", "Could not verify the active plugin.")
			return
		}
		entry, ok := registry.resolve(item.Plugin)
		if !ok || entry.Status != "enabled" || entry.Digest != item.Digest {
			writeAgentError(w, 409, "stale_digest", "The plugin changed or is disabled; request review with its current bundle.")
			return
		}
	}
	decision := map[string]string{"approve": "approved", "decline": "declined", "revoke": "revoked"}[parts[1]]
	item, err = s.resultReleases.Decide(parts[0], input.Status, decision)
	if errors.Is(err, resultrelease.ErrConflict) {
		writeAgentError(w, 409, "conflict", "This approval changed; reload it before deciding.")
		return
	}
	if err != nil {
		writeAgentError(w, 503, "state_unavailable", "Could not save your decision. Inspect the approval before retrying.")
		return
	}
	writeAgentJSON(w, 200, item)
}
