package mcpshim

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/torana-edge/torana-edge/internal/controlclient"
	"github.com/torana-edge/torana-edge/internal/mcpserver"
)

func TestStdioConsentProtocolBridge(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		for _, action := range []string{"accept", "decline", "cancel", "unsupported"} {
			t.Run(version+"/"+action, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				calls, confirmations, dialogs := 0, 0, 0
				var choice string
				handler, err := mcpserver.NewHandler(mcpserver.Options{Token: func() string { return "private-token" },
					Dispatch: func(context.Context, string, json.RawMessage) (mcpserver.Result, error) {
						calls++
						return mcpserver.Result{OK: true, Status: "pending_confirmation", Consent: &mcpserver.Consent{Message: "Before: enabled. After: disabled."}}, nil
					},
					SealConsent: func(context.Context, string, json.RawMessage, *mcpserver.Consent) (string, error) {
						return "private-state", nil
					},
					ResolveConsent: func(_ context.Context, _ string, _ json.RawMessage, state, action string) (mcpserver.Result, error) {
						confirmations++
						choice = action
						if state != "private-state" {
							t.Error("state was not relayed intact")
						}
						return mcpserver.Result{OK: true, Status: "completed"}, nil
					}})
				if err != nil {
					t.Fatal(err)
				}
				httpServer := httptest.NewServer(handler)
				defer httpServer.Close()
				defer handler.Shutdown(context.Background())
				operator, err := controlclient.New(httpServer.URL, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer operator.Close()
				local, remote := mcp.NewInMemoryTransports()
				done := make(chan error, 1)
				go func() { done <- Run(ctx, operator, "private-token", local) }()
				options := &mcp.ClientOptions{}
				if action != "unsupported" {
					options.ElicitationHandler = func(_ context.Context, request *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
						dialogs++
						if !strings.Contains(request.Params.Message, "Before: enabled") {
							t.Error("summary missing")
						}
						return &mcp.ElicitResult{Action: action}, nil
					}
				}
				client := mcp.NewClient(&mcp.Implementation{Name: "harness", Version: "1"}, options)
				session, err := client.Connect(ctx, remote, &mcp.ClientSessionOptions{ProtocolVersion: version})
				if err != nil {
					t.Fatal(err)
				}
				defer session.Close()
				if session.InitializeResult().ProtocolVersion != version {
					t.Fatalf("negotiated %s, want %s", session.InitializeResult().ProtocolVersion, version)
				}
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "torana_invoke", Arguments: map[string]any{"namespace": "logger", "operation": "_disable"}})
				if err != nil || result.IsError {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				encoded, _ := json.Marshal(result)
				if strings.Contains(string(encoded), "private-state") || strings.Contains(string(encoded), "private-token") {
					t.Fatal("private protocol data leaked to final tool output")
				}
				if calls != 1 {
					t.Fatalf("redispatched original call: %d", calls)
				}
				if action == "unsupported" {
					if confirmations != 0 || dialogs != 0 {
						t.Fatal("unsupported client confirmed")
					}
				} else if confirmations != 1 || dialogs != 1 || choice != action {
					t.Fatalf("confirmations=%d dialogs=%d choice=%q result=%s", confirmations, dialogs, choice, encoded)
				}
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("shim failed to stop")
				}
			})
		}
	}
}
