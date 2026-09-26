// Package mcpshim exposes the host's fixed MCP tools over a local transport.
// Tool definitions and instructions come from the host, not a second catalog.
package mcpshim

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/torana-edge/torana-edge/internal/controlclient"
	"github.com/torana-edge/torana-edge/internal/mcpserver"
)

func Run(ctx context.Context, client *controlclient.Client, token string, transport mcp.Transport) error {
	httpClient := client.MCPHTTPClient(token)
	defer httpClient.CloseIdleConnections()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Inline input requests let the shim bridge each downstream protocol without
	// granting consent itself or attempting forbidden modern server callbacks.
	remoteClient := mcp.NewClient(&mcp.Implementation{Name: "torana-stdio", Version: "1"}, &mcp.ClientOptions{Logger: logger,
		Capabilities:   &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}}},
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}})
	remote, err := remoteClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: client.Address() + "/_torana/mcp", HTTPClient: httpClient}, &mcp.ClientSessionOptions{ProtocolVersion: "2026-07-28"})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("cannot connect to Torana MCP; check the running instance and enable MCP")
	}
	defer func() { _ = remote.Close() }()
	tools, err := remote.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("cannot read Torana MCP tools")
	}
	expected := map[string]bool{"torana_namespaces": true, "torana_describe": true, "torana_search": true, "torana_invoke": true}
	if len(tools.Tools) != len(expected) || tools.NextCursor != "" {
		return fmt.Errorf("Torana MCP returned an unexpected tool catalog")
	}
	init := remote.InitializeResult()
	server := mcp.NewServer(init.ServerInfo, &mcp.ServerOptions{Instructions: init.Instructions, Logger: logger})
	for _, tool := range tools.Tools {
		if tool == nil || !expected[tool.Name] {
			return fmt.Errorf("Torana MCP returned an unexpected tool catalog")
		}
		delete(expected, tool.Name)
		server.AddTool(tool, func(callCtx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if req.Params == nil {
				return nil, fmt.Errorf("tool arguments are required")
			}
			// Each leg negotiates its own protocol and client capabilities. In
			// particular, stdio's 2026 metadata must not override HTTP's version.
			meta := mcp.Meta{}
			for key, value := range req.Params.Meta {
				if !strings.HasPrefix(key, "io.modelcontextprotocol/") {
					meta[key] = value
				}
			}
			result, err := remote.CallTool(callCtx, &mcp.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments, Meta: meta, InputResponses: req.Params.InputResponses, RequestState: req.Params.RequestState})
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "Torana MCP is unavailable. Check the instance; restart this connection after changing its token."}}}, nil
			}
			if result.NeedsInput() {
				caps := req.ClientCapabilities()
				if caps == nil || caps.Elicitation == nil || (caps.Elicitation.Form == nil && caps.Elicitation.URL != nil) {
					return pendingOperatorConfirmation(), nil
				}
				if req.ProtocolVersion() >= "2026-07-28" {
					return result, nil
				}
				// Torana currently requests exactly one form. Refuse unexpected
				// input types, rather than blindly relaying URLs or sampling.
				if len(result.InputRequests) != 1 {
					return pendingOperatorConfirmation(), nil
				}
				responses := mcp.InputResponseMap{}
				for id, input := range result.InputRequests {
					form, ok := input.(*mcp.ElicitParams)
					if !ok || form.Mode != "form" {
						return pendingOperatorConfirmation(), nil
					}
					answer, err := req.Session.Elicit(callCtx, form)
					if err != nil || answer == nil {
						return pendingOperatorConfirmation(), nil
					}
					responses[id] = answer
				}
				result, err = remote.CallTool(callCtx, &mcp.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments, Meta: meta, InputResponses: responses, RequestState: result.RequestState})
				if err != nil || result.NeedsInput() {
					return pendingOperatorConfirmation(), nil
				}
			}
			return result, nil
		})
	}
	return server.Run(ctx, transport)
}

func pendingOperatorConfirmation() *mcp.CallToolResult {
	return &mcp.CallToolResult{StructuredContent: mcpserver.Result{OK: true, Status: "pending_confirmation", Summary: "Review the pending change in Torana's UI or CLI.", InterfaceVersion: 1}, Content: []mcp.Content{&mcp.TextContent{Text: "Review the pending change in Torana's UI or CLI. No change was applied."}}}
}
