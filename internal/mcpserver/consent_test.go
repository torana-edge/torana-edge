package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOfficialClientConsentAcrossProtocols(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		for _, action := range []string{"accept", "decline", "cancel", "unsupported"} {
			t.Run(version+"/"+action, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				dispatches, resolutions, dialogs := 0, 0, 0
				handler, err := NewHandler(Options{Token: func() string { return "test" },
					Dispatch: func(context.Context, string, json.RawMessage) (Result, error) {
						dispatches++
						return Result{OK: true, Status: "pending_confirmation", Consent: &Consent{ID: "private-id", Conversation: "private-conversation", Message: "Disable logger? Before: enabled. After: disabled."}}, nil
					},
					SealConsent: func(context.Context, string, json.RawMessage, *Consent) (string, error) { return "opaque-state", nil },
					ResolveConsent: func(_ context.Context, name string, raw json.RawMessage, state, choice string) (Result, error) {
						resolutions++
						if name != "torana_invoke" || state != "opaque-state" || !strings.Contains(string(raw), "_disable") {
							t.Error("confirmation lost its operation binding")
						}
						status := map[string]string{"accept": "applied", "decline": "dismissed", "cancel": "pending_confirmation"}[choice]
						return Result{OK: true, Status: status}, nil
					}})
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(handler)
				defer server.Close()
				defer handler.Shutdown(context.Background())
				options := &mcp.ClientOptions{}
				if action != "unsupported" {
					options.ElicitationHandler = func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
						dialogs++
						if req.Params.Mode != "form" || !strings.Contains(req.Params.Message, "Before: enabled") {
							t.Error("missing host confirmation summary")
						}
						return &mcp.ElicitResult{Action: action}, nil
					}
				}
				client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, options)
				session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: &http.Client{Transport: tokenTransport{token: "test"}}}, &mcp.ClientSessionOptions{ProtocolVersion: version})
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
				for _, private := range []string{"private-id", "private-conversation", "opaque-state", "Before: enabled"} {
					if strings.Contains(string(encoded), private) {
						t.Errorf("private confirmation material in final tool response: %s", private)
					}
				}
				if dispatches != 1 {
					t.Errorf("redispatched consumed tool evidence: %d", dispatches)
				}
				if action == "unsupported" {
					if resolutions != 0 || dialogs != 0 {
						t.Fatal("unsupported client granted consent")
					}
				} else if resolutions != 1 || dialogs != 1 {
					t.Fatalf("resolutions=%d dialogs=%d", resolutions, dialogs)
				}
			})
		}
	}
}
