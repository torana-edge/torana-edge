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
)

func Run(ctx context.Context, client *controlclient.Client, token string, transport mcp.Transport) error {
	httpClient := client.MCPHTTPClient(token)
	defer httpClient.CloseIdleConnections()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	remoteClient := mcp.NewClient(&mcp.Implementation{Name: "torana-stdio", Version: "1"}, &mcp.ClientOptions{Logger: logger})
	remote, err := remoteClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: client.Address() + "/_torana/mcp", HTTPClient: httpClient}, nil)
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
			return result, nil
		})
	}
	return server.Run(ctx, transport)
}
