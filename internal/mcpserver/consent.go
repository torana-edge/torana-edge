package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const consentInputID = "confirm"

func dispatchWithConsent(ctx context.Context, options Options, req *mcp.CallToolRequest, name string, raw json.RawMessage) (Result, error) {
	if req.Params.RequestState != "" || len(req.Params.InputResponses) != 0 {
		response, ok := req.Params.InputResponses[consentInputID].(*mcp.ElicitResult)
		if options.ResolveConsent == nil || req.Params.RequestState == "" || len(req.Params.InputResponses) != 1 || !ok || response == nil {
			return Result{Error: &DomainError{Code: "invalid_confirmation", Message: "Confirmation is unavailable; review the pending change in Torana."}}, nil
		}
		return options.ResolveConsent(ctx, name, raw, req.Params.RequestState, response.Action)
	}
	return options.Dispatch(ctx, name, raw)
}

// Unsupported dialogs retain the persistent UI/CLI proposal, never approval.
func elicitConsent(ctx context.Context, options Options, req *mcp.CallToolRequest, name string, raw json.RawMessage, output Result) (*mcp.CallToolResult, Result, error) {
	if output.Consent == nil || options.SealConsent == nil || options.ResolveConsent == nil {
		return nil, output, nil
	}
	capabilities := req.ClientCapabilities()
	if capabilities == nil || capabilities.Elicitation == nil {
		return nil, output, nil
	}
	capability := capabilities.Elicitation
	if capability.Form == nil && capability.URL != nil {
		return nil, output, nil
	}
	state, err := options.SealConsent(ctx, name, raw, output.Consent)
	if err != nil {
		return nil, Result{}, err
	}
	params := &mcp.ElicitParams{Mode: "form", Message: output.Consent.Message, RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}}}
	if req.ProtocolVersion() >= "2026-07-28" {
		return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{consentInputID: params}, RequestState: state}, output, nil
	}
	answer, err := req.Session.Elicit(ctx, params)
	if err != nil || answer == nil {
		return nil, output, nil
	}
	resolved, err := options.ResolveConsent(ctx, name, raw, state, answer.Action)
	return nil, resolved, err
}
