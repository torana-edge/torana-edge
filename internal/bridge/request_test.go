package bridge

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/format"
	_ "github.com/torana-edge/torana-edge/internal/format/anthropic"
	_ "github.com/torana-edge/torana-edge/internal/format/gemini"
	_ "github.com/torana-edge/torana-edge/internal/format/openai"
)

var requestFixtures = map[Protocol]string{
	OpenAIChat:       `{"model":"source","messages":[{"role":"system","content":"Be precise"},{"role":"user","content":"weather?"},{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"weather","arguments":"{\"n\":9007199254740993}"}}]},{"role":"tool","tool_call_id":"call_a","content":"sunny"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}],"max_tokens":128}`,
	OpenAIResponses:  `{"model":"source","instructions":"Be precise","input":[{"type":"message","role":"user","content":"weather?"},{"type":"function_call","id":"item_a","call_id":"call_a","name":"weather","arguments":"{\"n\":9007199254740993}"},{"type":"function_call_output","call_id":"call_a","output":"sunny"}],"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"max_output_tokens":128}`,
	Anthropic:        `{"model":"source","system":"Be precise","messages":[{"role":"user","content":"weather?"},{"role":"assistant","content":[{"type":"tool_use","id":"call_a","name":"weather","input":{"n":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_a","content":"sunny"}]}],"tools":[{"name":"weather","input_schema":{"type":"object"}}],"max_tokens":128}`,
	Gemini:           `{"systemInstruction":{"parts":[{"text":"Be precise"}]},"contents":[{"role":"user","parts":[{"text":"weather?"}]},{"role":"model","parts":[{"functionCall":{"id":"call_a","name":"weather","args":{"n":9007199254740993}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call_a","name":"weather","response":{"content":"sunny"}}}]}],"tools":[{"functionDeclarations":[{"name":"weather","parameters":{"type":"object"}}]}],"generationConfig":{"maxOutputTokens":128}}`,
	GeminiCodeAssist: `{"model":"source","project":"source-project","request":{"systemInstruction":{"parts":[{"text":"Be precise"}]},"contents":[{"role":"user","parts":[{"text":"weather?"}]},{"role":"model","parts":[{"functionCall":{"id":"call_a","name":"weather","args":{"n":9007199254740993}}}]},{"role":"model","parts":[{"functionResponse":{"id":"call_a","name":"weather","response":{"output":"sunny"}}}]}],"tools":[{"functionDeclarations":[{"name":"weather","parameters":{"type":"object"}}]}],"generationConfig":{"maxOutputTokens":128}}}`,
}

func requestPath(p Protocol) string {
	path, _, _ := Endpoint(p, "source", false)
	return path
}

func TestRequestTranslationToolConversationMatrix(t *testing.T) {
	for from, raw := range requestFixtures {
		for to := range requestFixtures {
			if from == to {
				continue
			}
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				chat, err := ParseRequest(from, []byte(raw), requestPath(from))
				if err != nil {
					t.Fatal(err)
				}
				before, _ := format.Lookup(from.Format()).Request.Marshal(chat)
				projected, err := ProjectRequest(chat, from, to, RequestOptions{MaxTokens: 256, Project: "destination-project"})
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := format.Lookup(to.Format()).Request.Marshal(projected)
				if err != nil {
					t.Fatal(err)
				}
				after, _ := format.Lookup(from.Format()).Request.Marshal(chat)
				if !bytes.Equal(before, after) {
					t.Fatal("source request was mutated")
				}
				if !bytes.Contains(encoded, []byte("9007199254740993")) {
					t.Fatalf("arguments lost precision: %s", encoded)
				}
				reparsed, err := ParseRequest(to, encoded, requestPath(to))
				if err != nil {
					t.Fatalf("target cannot parse %s: %v", encoded, err)
				}
				calls, results, systems := 0, 0, 0
				for _, m := range reparsed.Messages {
					if m.Role == engine.RoleSystem {
						systems++
					}
					for _, b := range m.Blocks {
						if b.ToolUse != nil {
							calls++
							if b.ToolUse.ID != "call_a" || b.ToolUse.Name != "weather" {
								t.Fatalf("call identity changed: %+v", b.ToolUse)
							}
						}
						if b.ToolResult != nil {
							results++
							if b.ToolResult.ToolCallID != "call_a" {
								t.Fatalf("result identity changed: %+v", b.ToolResult)
							}
						}
					}
				}
				if calls != 1 || results != 1 || systems != 1 {
					t.Fatalf("lost conversation facts: calls=%d results=%d systems=%d; %s", calls, results, systems, encoded)
				}
				if reparsed.MaxTokens == nil || *reparsed.MaxTokens != 128 {
					t.Fatalf("lost token limit: %s", encoded)
				}
			})
		}
	}
}

func TestRequestBridgeRejectsSemanticLoss(t *testing.T) {
	rows := []struct {
		name     string
		from, to Protocol
		body     string
	}{
		{"cache marker", Anthropic, OpenAIChat, `{"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}]}`},
		{"signed thinking", Anthropic, OpenAIChat, `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"secret"}]}]}`},
		{"server state", OpenAIResponses, Anthropic, `{"input":"hello","previous_response_id":"secret","max_output_tokens":5}`},
		{"unknown top level", OpenAIChat, Anthropic, `{"messages":[{"role":"user","content":"x"}],"provider_secret":"do not echo"}`},
		{"strict tool", OpenAIChat, Anthropic, `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"t","strict":true,"parameters":{}}}]}`},
		{"unknown nested tool constraint", OpenAIChat, Anthropic, `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"t","parameters":{},"future_guarantee":true}}]}`},
		{"opaque item", OpenAIResponses, OpenAIChat, `{"input":[{"type":"reasoning","encrypted_content":"secret"}]}`},
		{"unmatched tool result", OpenAIChat, Anthropic, `{"messages":[{"role":"tool","tool_call_id":"missing","content":"x"}]}`},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			chat, err := ParseRequest(row.from, []byte(row.body), requestPath(row.from))
			if err == nil {
				_, err = ProjectRequest(chat, row.from, row.to, RequestOptions{MaxTokens: 16})
			}
			if err == nil {
				t.Fatal("silently accepted semantic loss")
			}
			if bytes.Contains([]byte(err.Error()), []byte("secret")) {
				t.Fatalf("request value exposed: %v", err)
			}
		})
	}
}

func TestRequestBridgeRequiresExplicitAnthropicLimit(t *testing.T) {
	chat, err := ParseRequest(OpenAIChat, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), requestPath(OpenAIChat))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ProjectRequest(chat, OpenAIChat, Anthropic, RequestOptions{}); err == nil {
		t.Fatal("invented token limit")
	}
	out, err := ProjectRequest(chat, OpenAIChat, Anthropic, RequestOptions{MaxTokens: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens == nil || *out.MaxTokens != 1024 {
		t.Fatal("did not use operator default")
	}
}

func TestBridgeToolChoiceAndParallelMapping(t *testing.T) {
	for i := 0; i < 30; i++ {
		chat, err := ParseRequest(OpenAIChat, []byte(`{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"lookup"}},"parallel_tool_calls":false,"max_tokens":8}`), requestPath(OpenAIChat))
		if err != nil {
			t.Fatal(err)
		}
		out, err := ProjectRequest(chat, OpenAIChat, Anthropic, RequestOptions{})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := format.Lookup("anthropic").Request.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		var obj struct {
			Choice struct {
				Type     string `json:"type"`
				Name     string `json:"name"`
				Disabled bool   `json:"disable_parallel_tool_use"`
			} `json:"tool_choice"`
		}
		if json.Unmarshal(raw, &obj) != nil || obj.Choice.Type != "tool" || obj.Choice.Name != "lookup" || !obj.Choice.Disabled {
			t.Fatalf("lost tool selection: %s", raw)
		}
	}
}
