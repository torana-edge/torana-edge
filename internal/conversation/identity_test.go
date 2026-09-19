package conversation

import (
	"net/http"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
)

func testChat(t *testing.T, extensions string) *engine.ChatRequest {
	t.Helper()
	chat := &engine.ChatRequest{Messages: []engine.Message{{
		Role:   engine.RoleUser,
		Blocks: []engine.Block{{Text: &engine.TextBlock{Text: "hello"}}},
	}}}
	if extensions != "" {
		var err error
		chat.ProviderExtensions, err = engine.ParseOptionalJSONObject([]byte(extensions))
		if err != nil {
			t.Fatal(err)
		}
	}
	return chat
}

func TestResolveIdentityPrefersHarnessHeaderAndStaysStableAcrossContent(t *testing.T) {
	headers := http.Header{"X-Claude-Code-Session-Id": []string{"session-123"}}
	first := testChat(t, "")
	second := testChat(t, "")
	second.Messages[0].Blocks[0].Text.Text = "compacted replacement"

	a := ResolveIdentity(headers, first)
	b := ResolveIdentity(headers, second)
	if a.ID == "" || a.ID != b.ID || a.Source != "claude-code-session" {
		t.Fatalf("identities = %#v %#v", a, b)
	}
	if a.ID == "session-123" {
		t.Fatal("raw harness identity leaked into the public label")
	}
}

func TestResolveIdentityProviderConventions(t *testing.T) {
	tests := []struct {
		name, extensions, source string
		prepare                  func(*engine.ChatRequest)
	}{
		{
			name:       "code assist",
			extensions: `{"project":"p","request":{"sessionId":"agy-session"}}`,
			source:     "gemini-code-assist-session",
			prepare:    func(c *engine.ChatRequest) { c.CodeAssist = true },
		},
		{
			name:       "responses string",
			extensions: `{"conversation":"conv_123"}`,
			source:     "openai-responses-conversation",
			prepare:    func(c *engine.ChatRequest) { c.OpenAIVariant = engine.OpenAIResponses },
		},
		{
			name:       "responses object",
			extensions: `{"conversation":{"id":"conv_456"}}`,
			source:     "openai-responses-conversation",
			prepare:    func(c *engine.ChatRequest) { c.OpenAIVariant = engine.OpenAIResponses },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chat := testChat(t, tc.extensions)
			tc.prepare(chat)
			got := ResolveIdentity(nil, chat)
			if got.ID == "" || got.Source != tc.source {
				t.Fatalf("ResolveIdentity = %#v", got)
			}
		})
	}
}

func TestResolveIdentityIgnoresChangingResponseParentWhenHistoryHasRoot(t *testing.T) {
	chat := testChat(t, `{"previous_response_id":"resp_123"}`)
	chat.OpenAIVariant = engine.OpenAIResponses
	got := ResolveIdentity(nil, chat)
	if got.Source != "content-root" || got.ID != engine.ConversationID(chat) {
		t.Fatalf("ResolveIdentity = %#v", got)
	}
}

func TestResolveIdentityUsesResponseParentForDeltaOnlyToolOutput(t *testing.T) {
	chat, err := format.Lookup("openai").Request.Unmarshal([]byte(`{
		"model":"gpt-test",
		"previous_response_id":"resp_123",
		"input":[{"type":"function_call_output","call_id":"call-1","output":"result"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	got := ResolveIdentity(nil, chat)
	if got.ID == "" || got.Source != "openai-responses-parent" {
		t.Fatalf("ResolveIdentity = %#v", got)
	}
}

func TestResolveIdentityUsesCodexClientMetadataAcrossDeltaTurns(t *testing.T) {
	build := func(parent string) *engine.ChatRequest {
		raw := `{"client_metadata":{"thread_id":"thread-stable"},"previous_response_id":"` + parent + `"}`
		chat := testChat(t, raw)
		chat.OpenAIVariant = engine.OpenAIResponses
		chat.Messages = []engine.Message{{Role: engine.RoleTool, Blocks: []engine.Block{{ToolResult: &engine.ToolResultBlock{
			ToolCallID: "call-1", Content: []engine.ToolResultContentBlock{{Text: "result"}},
		}}}}}
		return chat
	}
	first := ResolveIdentity(nil, build("resp-1"))
	second := ResolveIdentity(nil, build("resp-2"))
	if first.ID == "" || first.ID != second.ID || first.Source != "codex-client-thread" {
		t.Fatalf("identities = %#v %#v", first, second)
	}
}

func TestResolveIdentitySourceNamespacesDoNotAlias(t *testing.T) {
	claude := ResolveIdentity(http.Header{"X-Claude-Code-Session-Id": []string{"same"}}, testChat(t, ""))
	codex := ResolveIdentity(http.Header{"X-Codex-Thread-Id": []string{"same"}}, testChat(t, ""))
	if claude.ID == codex.ID {
		t.Fatal("unrelated identity namespaces aliased")
	}
}

func TestResolveIdentityFallsBackAndRejectsOversizedExternalID(t *testing.T) {
	chat := testChat(t, "")
	headers := http.Header{"Session-Id": []string{strings.Repeat("x", maxExternalIdentityBytes+1)}}
	got := ResolveIdentity(headers, chat)
	if got.Source != "content-root" || got.ID != engine.ConversationID(chat) {
		t.Fatalf("ResolveIdentity = %#v", got)
	}
}
