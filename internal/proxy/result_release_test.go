package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/controlclient"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/plugin"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/resultrelease"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestModelRequestsHumanReleaseWithoutGrantingConsent(t *testing.T) {
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	s, err := New(Config{Port: "8080", Providers: provider.DefaultConfig()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	item, _, err := s.resultReleases.Observe(resultrelease.Scope{Conversation: "session-a", Plugin: "pii", Digest: "sha256:bundle", CallID: "call-a", ContentHash: strings.Repeat("a", 64)}, true)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := buildNamespaceRegistry(nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := newNamespaceAccessPolicy(registry, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := operationDispatch{policy: policy, propose: s.proposeNamespaceOperation}
	raw := json.RawMessage(`{"namespace":"torana","operation":"redactions.request_release","input":{"reference":"` + item.Reference + `"}}`)
	for _, conversation := range []string{"session-b", "session-a"} {
		result, err := d.invoke(context.Background(), raw, plugin.MCPBinding{Bound: true, ConversationID: conversation, CallID: "mcp-call"})
		if err != nil {
			t.Fatal(err)
		}
		if conversation == "session-b" {
			if result.Error == nil || result.Error.Code != "not_found" {
				t.Fatalf("cross-session=%+v error=%+v", result, result.Error)
			}
		} else if !result.OK || result.Status != "pending" || result.Consent != nil {
			t.Fatalf("request=%+v", result)
		}
	}
	item, err = s.resultReleases.Get(item.Reference)
	if err != nil || item.Status != "pending" {
		t.Fatalf("request self-approved=%+v %v", item, err)
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ConversationID: "session-a"})
	tr := &pb.RequestToolResultBlock{ToolCallId: "call-a", Content: []*pb.ToolResultContentBlock{{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "ordinary synthetic content"}}}}}
	observed, refusal := s.observeToolResultRelease(ctx, "pii", "sha256:bundle", tr, nil, &sdk.ToolResultReleaseReason{Kind: "scan_failure"})
	if refusal != nil {
		t.Fatal(refusal)
	}
	var info struct {
		Reference string `json:"reference"`
		Approved  bool   `json:"approved"`
	}
	if json.Unmarshal(observed, &info) != nil || info.Approved {
		t.Fatalf("observation=%s", observed)
	}
	for _, callID := range []string{"", "torana_gemini_semantic_0"} {
		tr.ToolCallId = callID
		raw, refusal := s.observeToolResultRelease(ctx, "pii", "sha256:bundle", tr, nil, &sdk.ToolResultReleaseReason{Kind: "scan_failure"})
		if refusal != nil || string(raw) != `{"reference":"","approved":false}` {
			t.Fatalf("ambiguous call got review reference: %s %v", raw, refusal)
		}
	}
	// Operator guard rejects browser-origin writes without the local request
	// marker; MCP never exposes this route or the approval/revoke operations.
	request := httptest.NewRequest(http.MethodPost, resultReleaseAPIPath+"/"+item.Reference+"/decline", strings.NewReader(`{"expected_status":"pending"}`))
	request.Header.Set("Origin", "https://untrusted.example")
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, request)
	if recorder.Code < 400 {
		t.Fatalf("untrusted origin=%d", recorder.Code)
	}
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	client, err := controlclient.New(httpServer.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, _, err := client.JSON(context.Background(), http.MethodPost, resultReleaseAPIPath+"/"+item.Reference+"/decline", []byte(`{"expected_status":"pending"}`), ""); err == nil {
		t.Fatal("local mutation marker alone granted a decision")
	}
	item, err = s.resultReleases.Get(item.Reference)
	if err != nil || item.Status != "pending" {
		t.Fatal("missing proof changed approval state", err)
	}
	if _, err := runControlCLI(t, httpServer.URL, "", "approvals", "decline", item.Reference); err == nil {
		t.Fatal("CLI decision did not require a terminal")
	}
	if _, err := runControlCLI(t, httpServer.URL, "", "approvals", "decline", item.Reference, "--yes"); err == nil {
		t.Fatal("--yes bypassed interactive confirmation")
	}
	if err := reviewResultDecision(t, httpServer.URL, item.Reference, "pending", "decline"); err != nil {
		t.Fatal(err)
	}
	item, err = s.resultReleases.Get(item.Reference)
	if err != nil || item.Status != "declined" {
		t.Fatalf("CLI decline=%+v %v", item, err)
	}
	for _, operation := range []string{"redactions.approve", "redactions.revoke", "approvals.approve"} {
		if policy.ModelReachable("torana", operation) != "never" {
			t.Fatalf("model can approve via %s", operation)
		}
	}
}

func TestReleaseReviewAttachesFromUnboundMCPTranscript(t *testing.T) {
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	cfg := provider.DefaultConfig()
	cfg.MCP.Enabled = true
	s, err := New(Config{Port: "8080", Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	item, _, err := s.resultReleases.Observe(resultrelease.Scope{Conversation: "session-a", Plugin: "pii", Digest: "sha256:bundle", CallID: "read-a", ContentHash: strings.Repeat("a", 64)}, true)
	if err != nil {
		t.Fatal(err)
	}
	args, err := engine.ParseRequiredJSONObject([]byte(`{"namespace":"torana","operation":"redactions.request_release","input":{"reference":"` + item.Reference + `"}}`))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := s.mcpPolicy()
	if err != nil {
		t.Fatal(err)
	}
	d := operationDispatch{policy: policy, propose: s.proposeNamespaceOperation, sealPending: s.sealTranscriptTicket}
	result, err := d.invoke(context.Background(), args.Bytes(), plugin.MCPBinding{})
	if err != nil || result.Ticket == "" || result.Consent != nil {
		t.Fatalf("unbound review=%+v %v", result, err)
	}
	stored, err := s.resultReleases.Get(item.Reference)
	if err != nil || stored.Status != "withheld" {
		t.Fatalf("unbound request changed state=%+v %v", stored, err)
	}
	raw, _ := json.Marshal(result)
	request := &engine.ChatRequest{Messages: []engine.Message{
		{Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{ID: "mcp-a", Name: "mcp__torana__torana_invoke", Arguments: args}}}},
		{Role: engine.RoleUser, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{ToolCallID: "mcp-a", Content: []engine.ToolResultContentBlock{{Text: string(raw)}}}}}},
	}}
	s.observeTranscriptOperations(context.Background(), request, "session-a", []string{"torana"})
	stored, err = s.resultReleases.Get(item.Reference)
	if err != nil || stored.Status != "pending" {
		t.Fatalf("bound review=%+v %v", stored, err)
	}
	s.observeTranscriptOperations(context.Background(), request, "session-a", []string{"torana"})
	stored, _ = s.resultReleases.Get(item.Reference)
	if stored.Status != "pending" {
		t.Fatal("transcript replay approved the result")
	}
}

func TestHumanReleaseRequiresActiveExactBundleAndCanBeRevoked(t *testing.T) {
	requireWASM(t, fixturesDir+"/test-http-server/plugin.wasm")
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	cfg := provider.DefaultConfig()
	cfg.Plugins = provider.PluginsConfig{Dir: fixturesDir, Order: []string{"test-http-server"}, AllowUnapproved: true}
	s, err := New(Config{Port: "8080", Providers: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(context.Background())
	registry, err := s.currentNamespaceRegistry()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := registry.resolve("test-http-server")
	if !ok || entry.Status != "enabled" {
		t.Fatalf("fixture not active: %+v", entry)
	}
	ctx := context.WithValue(context.Background(), reqStateKey{}, &reqState{ConversationID: "human-session"})
	tr := &pb.RequestToolResultBlock{ToolCallId: "read-a", Content: []*pb.ToolResultContentBlock{{Kind: &pb.ToolResultContentBlock_Text{Text: &pb.ToolResultTextBlock{Text: "synthetic benign text"}}}}}
	observe := func(digest string) sdk.ToolResultReleaseInfo {
		t.Helper()
		raw, refusal := s.observeToolResultRelease(ctx, entry.Name, digest, tr, &pb.RequestToolUseBlock{Id: tr.ToolCallId, Name: "Read", ArgumentsJson: []byte(`{"file_path":"src/synthetic.txt"}`)}, &sdk.ToolResultReleaseReason{Kind: "findings", Findings: []sdk.ToolResultReleaseFinding{{Type: "api_key", Line: 2}}})
		if refusal != nil {
			t.Fatal(refusal)
		}
		var info sdk.ToolResultReleaseInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			t.Fatal(err)
		}
		return info
	}
	current := observe(entry.Digest)
	contextRecord, err := s.resultReleases.Get(current.Reference)
	if err != nil || contextRecord.Review == nil || contextRecord.Review.ToolName != "Read" || contextRecord.Review.FilePath != "src/synthetic.txt" || contextRecord.Review.Reason.Findings[0].Line != 2 {
		t.Fatalf("missing host context: %+v %v", contextRecord, err)
	}
	if _, err := s.resultReleases.Request(current.Reference, "human-session"); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	if err := reviewResultDecision(t, httpServer.URL, current.Reference, "pending", "approve"); err != nil {
		t.Fatal(err)
	}
	if !observe(entry.Digest).Approved {
		t.Fatal("human approval did not reach the plugin host API")
	}
	tr.ToolCallId = "read-b"
	if observe(entry.Digest).Approved {
		t.Fatal("new tool call inherited approval")
	}
	tr.ToolCallId = "read-a"
	tr.Content[0].GetText().Text = "changed benign text"
	if observe(entry.Digest).Approved {
		t.Fatal("changed content inherited approval")
	}
	tr.Content[0].GetText().Text = "synthetic benign text"
	stale := observe("sha256:stale")
	if _, err := s.resultReleases.Request(stale.Reference, "human-session"); err != nil {
		t.Fatal(err)
	}
	if err := reviewResultDecision(t, httpServer.URL, stale.Reference, "pending", "approve"); err == nil {
		t.Fatal("approved a stale bundle")
	}
	if err := reviewResultDecision(t, httpServer.URL, current.Reference, "approved", "revoke"); err != nil {
		t.Fatal(err)
	}
	if observe(entry.Digest).Approved {
		t.Fatal("revocation did not reach the plugin host API")
	}
}

func TestReleaseReviewRetainsOnlyKnownReadPaths(t *testing.T) {
	reason := &sdk.ToolResultReleaseReason{Kind: "scan_failure"}
	result := &pb.RequestToolResultBlock{ToolName: "Read"}
	for _, test := range []struct{ name, args, want string }{
		{"Read", `{"file_path":"src/config.json","value":"must-not-store"}`, "src/config.json"},
		{"read_file", `{"path":"src/config.json"}`, "src/config.json"},
		{"Bash", `{"command":"cat src/config.json; echo secret","path":"src/config.json"}`, ""},
		{"Read", `{"file_path":"https://user:secret@example.com/file"}`, ""},
		{"Read", `{"file_path":"injected\npath"}`, ""},
	} {
		context := releaseReviewContext(result, &pb.RequestToolUseBlock{Name: test.name, ArgumentsJson: []byte(test.args)}, reason)
		if context.FilePath != test.want || context.ToolName != test.name {
			t.Fatalf("%s: %+v", test.name, context)
		}
		raw, _ := json.Marshal(context)
		if strings.Contains(string(raw), "must-not-store") || strings.Contains(string(raw), "echo secret") {
			t.Fatal("arbitrary tool arguments retained")
		}
	}
}

// Tests deliberately obtain a CSRF session as an operator HTTP client. This
// is not proof of a human: unrestricted same-user clients remain issue #467.
func reviewResultDecision(t *testing.T, addr, ref, status, action string) error {
	t.Helper()
	client, err := controlclient.New(addr, time.Second*10)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.BeginApprovalSession(context.Background()); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"expected_status": status})
	_, _, err = client.JSON(context.Background(), http.MethodPost, resultReleaseAPIPath+"/"+ref+"/"+action, body, "")
	return err
}

func TestApprovalSessionRejectsMissingForgedExpiredAndRestartedProof(t *testing.T) {
	s := &Server{controlPlaneRevisionKey: "synthetic-test-instance"}
	issue := httptest.NewRecorder()
	s.handleApprovalSession(issue, httptest.NewRequest(http.MethodPost, approvalSessionPath, nil))
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(issue.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	cookies := issue.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || issue.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("session cookie is not private or bounded")
	}
	request := httptest.NewRequest(http.MethodPost, resultReleaseAPIPath+"/tr_"+strings.Repeat("a", 64)+"/approve", nil)
	if s.validApprovalSession(request) {
		t.Fatal("missing proof accepted")
	}
	request.AddCookie(cookies[0])
	if s.validApprovalSession(request) {
		t.Fatal("cookie without header accepted")
	}
	request.Header.Set(approvalSessionHeader, session.Token)
	if !s.validApprovalSession(request) {
		t.Fatal("fresh matching proof rejected")
	}
	request.Header.Set(approvalSessionHeader, session.Token+"forged")
	if s.validApprovalSession(request) {
		t.Fatal("mismatching proof accepted")
	}
	request.Header.Set(approvalSessionHeader, session.Token)
	s.controlPlaneRevisionKey = "another-instance"
	if s.validApprovalSession(request) {
		t.Fatal("proof survived server restart")
	}
	for _, expiration := range []int64{time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Hour).Unix()} {
		payload := fmt.Sprintf("%d.abcdefghijklmnop", expiration)
		token := payload + "." + s.approvalSessionMAC(payload)
		request.Header.Set("Cookie", approvalSessionCookie+"="+token)
		request.Header.Set(approvalSessionHeader, token)
		if s.validApprovalSession(request) {
			t.Fatal("invalid expiration accepted")
		}
	}
	for _, op := range resultReleaseAgentOperations() {
		if op.Risk != "read" {
			t.Fatalf("agent can discover decision: %s", op.ID)
		}
	}
}
