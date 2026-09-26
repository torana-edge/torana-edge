package proxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/auditlog"
	"github.com/torana-edge/torana-edge/internal/mcpserver"
)

func TestMCPConfirmationRetriesShareAdmissionLimitsAndSafeAudit(t *testing.T) {
	s := newMCPTestServer(t)
	cfg := s.GetConfig().Providers
	cfg.MCP.Enabled = true
	if err := s.SetProviders(cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	writer, err := auditlog.Open(auditlog.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	s.swapAuditWriter(writer)
	raw := json.RawMessage(`{"namespace":"logger","operation":"_disable","input":{"private-secret":"not-for-audit"}}`)
	state, err := s.sealMCPConsent(context.Background(), "torana_invoke", raw, &mcpserver.Consent{ID: "private-id", Conversation: "private-conversation"})
	if err != nil {
		t.Fatal(err)
	}
	s.mcpLimits.Update(1, 1)
	first, err := s.resolveMCPConsent(context.Background(), "torana_invoke", raw, state, "cancel")
	if err != nil || first.Status != "pending_confirmation" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := s.resolveMCPConsent(context.Background(), "torana_invoke", raw, state, "cancel")
	if err != nil || second.Error == nil || second.Error.Code != "rate_limited" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	s.swapAuditWriter(nil)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-secret", "not-for-audit", "private-id", "private-conversation", state} {
		if strings.Contains(string(data), private) {
			t.Fatalf("audit leaked %s", private)
		}
	}
	if strings.Count(string(data), `"type":"mcp_call"`) != 2 || !strings.Contains(string(data), `"error_code":"rate_limited"`) {
		t.Fatalf("audit=%s", data)
	}
}
