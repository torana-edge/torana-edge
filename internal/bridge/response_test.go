package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/engine"
)

func translateResponseForTest(from, to Protocol, body []byte, model string) ([]byte, error) {
	if to != OpenAIResponses || from == to {
		return TranslateResponse(from, to, body, model)
	}
	return TranslateResponseWithRequest(from, to, body, model, responsesClientRequestForTest())
}

func responsesClientRequestForTest() *engine.ChatRequest {
	return &engine.ChatRequest{
		Model:         "client-model",
		OpenAIVariant: engine.OpenAIResponses,
		Messages: []engine.Message{{Role: engine.RoleUser, Blocks: []engine.Block{{
			Text: &engine.TextBlock{Text: "request"},
		}}}},
	}
}

func TestTranslateResponseTextCrossProtocolMatrix(t *testing.T) {
	fixtures := map[Protocol]string{
		OpenAIChat:       `{"id":"chat_1","object":"chat.completion","model":"up-chat","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":4}}}`,
		OpenAIResponses:  `{"id":"resp_1","object":"response","model":"up-responses","status":"completed","output":[{"id":"msg_provider","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"input_tokens_details":{"cached_tokens":4}}}`,
		Anthropic:        `{"id":"msg_1","type":"message","role":"assistant","model":"up-anthropic","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":6,"cache_read_input_tokens":4,"cache_creation_input_tokens":0,"output_tokens":2}}`,
		Gemini:           `{"responseId":"gem_1","modelVersion":"up-gemini","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12,"cachedContentTokenCount":4}}`,
		GeminiCodeAssist: `{"response":{"responseId":"ca_1","modelVersion":"up-codeassist","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12,"cachedContentTokenCount":4}}}`,
	}
	protocols := []Protocol{OpenAIChat, OpenAIResponses, Anthropic, Gemini, GeminiCodeAssist}
	for _, from := range protocols {
		for _, to := range protocols {
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				input := []byte(fixtures[from])
				out, err := translateResponseForTest(from, to, input, "client-model")
				if err != nil {
					t.Fatalf("TranslateResponse: %v", err)
				}
				if from == to {
					if !bytes.Equal(out, input) {
						t.Fatalf("native response changed:\n%s", out)
					}
					return
				}
				got, err := parseCompletion(to, out)
				if err != nil {
					t.Fatalf("translated response is not valid %s: %v\n%s", to, err, out)
				}
				if got.Model != map[Protocol]string{
					OpenAIChat: "up-chat", OpenAIResponses: "up-responses", Anthropic: "up-anthropic",
					Gemini: "up-gemini", GeminiCodeAssist: "up-codeassist",
				}[from] || got.Finish != "stop" {
					t.Fatalf("model/finish = %q/%q", got.Model, got.Finish)
				}
				if len(got.Blocks) != 1 || got.Blocks[0].Text == nil || got.Blocks[0].Text.Text != "hello" {
					t.Fatalf("blocks = %#v", got.Blocks)
				}
				if got.Usage == nil || got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 2 || got.Usage.CacheReadTokens != 4 {
					t.Fatalf("usage = %#v", got.Usage)
				}
			})
		}
	}
}

func TestTranslateResponseToolsPreserveRawArgumentsAndIdentities(t *testing.T) {
	const large = `9007199254740993123456789`
	responses := []byte(`{"id":"resp_1","object":"response","model":"gpt","status":"completed","output":[` +
		`{"id":"fc_provider","type":"function_call","status":"completed","call_id":"call_provider","name":"lookup","arguments":"{ \"z\":` + large + `,\"a\":1}"}` +
		`],"usage":{"input_tokens":8,"output_tokens":3,"total_tokens":11}}`)

	for _, to := range []Protocol{OpenAIChat, Anthropic, Gemini, GeminiCodeAssist} {
		t.Run("responses_to_"+string(to), func(t *testing.T) {
			out, err := translateResponseForTest(OpenAIResponses, to, responses, "client-model")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(out, []byte(large)) {
				t.Fatalf("large integer lexeme was lost: %s", out)
			}
			got, err := parseCompletion(to, out)
			if err != nil {
				t.Fatal(err)
			}
			if got.Finish != "tool_calls" || len(got.Blocks) != 1 || got.Blocks[0].Tool == nil {
				t.Fatalf("tool response = %#v", got)
			}
			tool := got.Blocks[0].Tool
			if tool.CallID != "call_provider" || tool.Name != "lookup" || !bytes.Contains(tool.Args, []byte(large)) {
				t.Fatalf("tool = %#v args=%s", tool, tool.Args)
			}
		})
	}

	// A Responses output item ID and its call ID have different jobs. The
	// item ID stays distinct when creating the contract from a provider that
	// only had a call ID.
	anthropic := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[{"type":"tool_use","id":"call_same","name":"lookup","input":{"n":` + large + `}}],"stop_reason":"tool_use","stop_sequence":null}`)
	out, err := translateResponseForTest(Anthropic, OpenAIResponses, anthropic, "gpt")
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Output []struct {
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Output) != 1 || wire.Output[0].CallID != "call_same" || wire.Output[0].ID == "" || wire.Output[0].ID == wire.Output[0].CallID {
		t.Fatalf("Responses IDs not kept distinct: %s", out)
	}
	if !strings.Contains(wire.Output[0].Arguments, large) {
		t.Fatalf("arguments = %q", wire.Output[0].Arguments)
	}
}

func TestTranslateResponseToolCrossProtocolMatrix(t *testing.T) {
	const args = `{"z":9007199254740993123456789,"a":1e+09}`
	fixtures := map[Protocol]string{
		OpenAIChat:       `{"id":"chat","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_provider","type":"function","function":{"name":"lookup","arguments":"{\"z\":9007199254740993123456789,\"a\":1e+09}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		OpenAIResponses:  `{"id":"resp","object":"response","model":"m","status":"completed","output":[{"id":"fc_provider","type":"function_call","status":"completed","call_id":"call_provider","name":"lookup","arguments":"{\"z\":9007199254740993123456789,\"a\":1e+09}"}],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2}}`,
		Anthropic:        `{"id":"msg","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"call_provider","name":"lookup","input":` + args + `}],"stop_reason":"tool_use","stop_sequence":null}`,
		Gemini:           `{"responseId":"gem","modelVersion":"m","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_provider","name":"lookup","args":` + args + `}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`,
		GeminiCodeAssist: `{"response":{"responseId":"ca","modelVersion":"m","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_provider","name":"lookup","args":` + args + `}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`,
	}
	protocols := []Protocol{OpenAIChat, OpenAIResponses, Anthropic, Gemini, GeminiCodeAssist}
	for _, from := range protocols {
		for _, to := range protocols {
			if from == to {
				continue
			}
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				out, err := translateResponseForTest(from, to, []byte(fixtures[from]), "client-model")
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(out, []byte("9007199254740993123456789")) || !bytes.Contains(out, []byte("1e+09")) {
					t.Fatalf("argument number lexemes changed: %s", out)
				}
				got, err := parseCompletion(to, out)
				if err != nil {
					t.Fatal(err)
				}
				if got.Finish != "tool_calls" || len(got.Blocks) != 1 || got.Blocks[0].Tool == nil {
					t.Fatalf("tool completion = %#v", got)
				}
				tool := got.Blocks[0].Tool
				if tool.CallID != "call_provider" || tool.Name != "lookup" || string(tool.Args) != args {
					t.Fatalf("tool changed: %#v args=%s", tool, tool.Args)
				}
			})
		}
	}
}

func TestTranslateResponseUsageCacheDomains(t *testing.T) {
	anthropic := []byte(`{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":6,"cache_read_input_tokens":3,"cache_creation_input_tokens":1,"output_tokens":2}}`)
	out, err := translateResponseForTest(Anthropic, OpenAIResponses, anthropic, "gpt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"input_tokens":10`)) || !bytes.Contains(out, []byte(`"cached_tokens":3`)) || !bytes.Contains(out, []byte(`"cache_write_tokens":1`)) {
		t.Fatalf("Anthropic cache domain was not normalized: %s", out)
	}

	back, err := translateResponseForTest(OpenAIResponses, Anthropic, out, "claude")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range [][]byte{[]byte(`"input_tokens":6`), []byte(`"cache_read_input_tokens":3`), []byte(`"cache_creation_input_tokens":1`)} {
		if !bytes.Contains(back, marker) {
			t.Fatalf("OpenAI cache domain was not normalized: %s", back)
		}
	}

	for _, to := range []Protocol{OpenAIChat, Gemini, GeminiCodeAssist} {
		_, err := translateResponseForTest(Anthropic, to, anthropic, "m")
		var unsupportedErr *UnsupportedError
		if !errors.As(err, &unsupportedErr) || !strings.Contains(unsupportedErr.Feature, "cache write") {
			t.Fatalf("%s cache-write error = %v", to, err)
		}
	}
}

func TestTranslateResponseLengthStaysIncomplete(t *testing.T) {
	input := []byte(`{"id":"resp","object":"response","model":"gpt","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"id":"msg","type":"message","status":"incomplete","role":"assistant","content":[{"type":"output_text","text":"partial","annotations":[]}]}],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2}}`)
	for _, to := range []Protocol{OpenAIChat, Anthropic, Gemini, GeminiCodeAssist} {
		t.Run(string(to), func(t *testing.T) {
			out, err := translateResponseForTest(OpenAIResponses, to, input, "m")
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseCompletion(to, out)
			if err != nil {
				t.Fatal(err)
			}
			if got.Finish != "length" {
				t.Fatalf("finish = %q; body=%s", got.Finish, out)
			}
		})
	}
}

func TestTranslateResponseRejectsUnsupportedProviderSemantics(t *testing.T) {
	tests := []struct {
		name string
		from Protocol
		body string
	}{
		{"multiple choices", OpenAIChat, `{"model":"m","choices":[{"message":{"role":"assistant","content":"a"},"finish_reason":"stop"},{"message":{"role":"assistant","content":"b"},"finish_reason":"stop"}]}`},
		{"chat refusal", OpenAIChat, `{"model":"m","choices":[{"message":{"role":"assistant","content":null,"refusal":"declined"},"finish_reason":"stop"}]}`},
		{"responses reasoning", OpenAIResponses, `{"model":"m","status":"completed","output":[{"id":"r","type":"reasoning","summary":[]}]}`},
		{"responses failed", OpenAIResponses, `{"model":"m","status":"failed","error":{"code":"server_error","message":"secret"},"output":[]}`},
		{"anthropic thinking", Anthropic, `{"type":"message","role":"assistant","model":"m","content":[{"type":"thinking","thinking":"secret","signature":"secret-signature"}],"stop_reason":"end_turn"}`},
		{"multiple candidates", Gemini, `{"modelVersion":"m","candidates":[{"content":{"role":"model","parts":[{"text":"a"}]},"finishReason":"STOP"},{"content":{"role":"model","parts":[{"text":"b"}]},"finishReason":"STOP"}]}`},
		{"gemini signature", Gemini, `{"modelVersion":"m","candidates":[{"content":{"role":"model","parts":[{"text":"secret","thoughtSignature":"secret-signature"}]},"finishReason":"STOP"}]}`},
		{"codeassist metadata", GeminiCodeAssist, `{"response":{"modelVersion":"m","candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]},"traceId":"secret-trace"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			to := Anthropic
			if tc.from == Anthropic {
				to = OpenAIResponses
			}
			_, err := translateResponseForTest(tc.from, to, []byte(tc.body), "m")
			var unsupportedErr *UnsupportedError
			if !errors.As(err, &unsupportedErr) {
				t.Fatalf("error = %v, want UnsupportedError", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked a response value: %v", err)
			}
		})
	}
}

func TestTranslateResponseRejectsMalformedShapes(t *testing.T) {
	tests := []struct {
		name string
		from Protocol
		body string
	}{
		{"invalid json", Anthropic, `{"model":"secret"`},
		{"arguments not object", OpenAIChat, `{"model":"m","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"[1]"}}]},"finish_reason":"tool_calls"}]}`},
		{"missing terminal", Gemini, `{"modelVersion":"m","candidates":[{"content":{"role":"model","parts":[{"text":"x"}]}}]}`},
		{"cache exceeds input", OpenAIResponses, `{"model":"m","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"input_tokens_details":{"cached_tokens":3}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			to := Anthropic
			if tc.from == Anthropic {
				to = OpenAIResponses
			}
			_, err := translateResponseForTest(tc.from, to, []byte(tc.body), "m")
			if err == nil {
				t.Fatal("malformed response accepted")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked a response value: %v", err)
			}
		})
	}
}

func TestTranslateResponseModelSelection(t *testing.T) {
	input := []byte(`{"id":"m","type":"message","role":"assistant","model":"upstream-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null}`)
	out, err := translateResponseForTest(Anthropic, Gemini, input, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"modelVersion":"upstream-model"`)) {
		t.Fatalf("reported upstream model not used: %s", out)
	}
	out, err = translateResponseForTest(Anthropic, Gemini, input, "configured-model")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"modelVersion":"upstream-model"`)) {
		t.Fatalf("configured fallback replaced authoritative upstream model: %s", out)
	}
	missing := []byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null}`)
	out, err = translateResponseForTest(Anthropic, Gemini, missing, "configured-model")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"modelVersion":"configured-model"`)) {
		t.Fatalf("configured model fallback not used: %s", out)
	}
}

func TestTranslateResponseAcceptsRealisticOpenAIEnvelopes(t *testing.T) {
	responses := []byte(`{
  "id":"resp_67cb71b351908190a308f3859487620d06981a8637e6bc44",
  "object":"response","created_at":1741386163,"status":"completed","completed_at":1741386164,
  "error":null,"incomplete_details":null,"instructions":null,"max_output_tokens":null,
  "model":"gpt-6-astra","output":[{"type":"message","id":"msg_provider","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}],
  "parallel_tool_calls":true,"previous_response_id":null,"reasoning":{"effort":null,"summary":null},
  "store":true,"temperature":1.0,"text":{"format":{"type":"text"}},"tool_choice":"auto","tools":[],
  "top_p":1.0,"truncation":"disabled","usage":{"input_tokens":32,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":18,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":50},
  "user":null,"metadata":{},"background":false,"conversation":null,"max_tool_calls":null,
  "prompt":null,"prompt_cache_diagnostics":null,"prompt_cache_key":null,"prompt_cache_options":null,
  "prompt_cache_retention":null,"safety_identifier":null,"service_tier":"default","top_logprobs":0
}`)
	out, err := translateResponseForTest(OpenAIResponses, Anthropic, responses, "fallback")
	if err != nil {
		t.Fatalf("realistic Responses object was rejected: %v", err)
	}
	got, err := parseCompletion(Anthropic, out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "gpt-6-astra" || got.ID != "resp_67cb71b351908190a308f3859487620d06981a8637e6bc44" || got.Usage == nil || got.Usage.InputTokens != 32 || got.Usage.OutputTokens != 18 {
		t.Fatalf("translated completion = %#v", got)
	}

	chat := []byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1738960610,"model":"gpt-6-astra","system_fingerprint":"fp_provider","service_tier":"priority","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop","logprobs":null}],"usage":{"prompt_tokens":13,"completion_tokens":18,"total_tokens":31,"prompt_tokens_details":{"cached_tokens":0,"audio_tokens":0,"image_tokens":0,"text_tokens":0},"completion_tokens_details":{"accepted_prediction_tokens":0,"audio_tokens":0,"reasoning_tokens":0,"rejected_prediction_tokens":0,"text_tokens":0}}}`)
	if _, err := translateResponseForTest(OpenAIChat, Gemini, chat, "fallback"); err != nil {
		t.Fatalf("ordinary Chat serving metadata was rejected: %v", err)
	}
}

func TestTranslateResponseStrictJSONBeforeDecode(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{"duplicate top member", []byte(`{"model":"first","model":"secret","status":"completed","output":[]}`)},
		{"duplicate nested member", []byte(`{"model":"m","status":"completed","output":[{"id":"fc","type":"function_call","status":"completed","call_id":"c","name":"f","arguments":"{\"x\":1,\"x\":2}"}]}`)},
		{"lone surrogate", []byte(`{"model":"m","status":"completed","output":[{"id":"msg","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"\ud800"}]}]}`)},
		{"invalid utf8", append([]byte(`{"model":"m","status":"completed","output":[{"id":"msg","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"`), append([]byte{0xff}, []byte(`"}]}]}`)...)...)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := translateResponseForTest(OpenAIResponses, Anthropic, tc.body, "fallback")
			if err == nil {
				t.Fatal("invalid JSON was normalized and accepted")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked a response value: %v", err)
			}
		})
	}
}

func TestTranslateResponseRequiredStringsRejectNull(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","status":null,"output":[]}`,
		`{"model":"m","status":"completed","output":[{"id":"fc","type":"function_call","status":"completed","call_id":"c","name":null,"arguments":"{}"}]}`,
	} {
		if _, err := translateResponseForTest(OpenAIResponses, Anthropic, []byte(body), "fallback"); err == nil {
			t.Fatalf("required null string accepted: %s", body)
		}
	}
}

func TestTranslateResponseUsageOverflowAndDetailPolicy(t *testing.T) {
	accepted := []byte(`{"model":"m","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}}`)
	if _, err := translateResponseForTest(OpenAIResponses, Anthropic, accepted, "fallback"); err != nil {
		t.Fatalf("zero-valued documented detail rejected: %v", err)
	}

	tests := []struct {
		name string
		from Protocol
		body string
	}{
		{"openai total addition", OpenAIResponses, `{"model":"m","status":"completed","output":[],"usage":{"input_tokens":9223372036854775807,"output_tokens":1}}`},
		{"openai cache addition", OpenAIResponses, `{"model":"m","status":"completed","output":[],"usage":{"input_tokens":9223372036854775807,"output_tokens":0,"input_tokens_details":{"cached_tokens":9223372036854775807,"cache_write_tokens":1}}}`},
		{"anthropic input addition", Anthropic, `{"type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","usage":{"input_tokens":9223372036854775807,"cache_read_input_tokens":1,"output_tokens":0}}`},
		{"gemini total addition", Gemini, `{"modelVersion":"m","candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9223372036854775807,"candidatesTokenCount":1}}`},
		{"reasoning token detail", OpenAIResponses, `{"model":"m","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":3}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			to := Anthropic
			if tc.from == Anthropic {
				to = OpenAIResponses
			}
			if _, err := translateResponseForTest(tc.from, to, []byte(tc.body), "fallback"); err == nil {
				t.Fatal("lossy or overflowing usage accepted")
			}
		})
	}
}

func TestTranslateResponseSynthesizesUniqueResponseIDs(t *testing.T) {
	anthropic := []byte(`{"type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	chat := []byte(`{"object":"chat.completion","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	for _, to := range []Protocol{OpenAIChat, OpenAIResponses, Anthropic, Gemini, GeminiCodeAssist} {
		from, body := Anthropic, anthropic
		if to == Anthropic {
			from, body = OpenAIChat, chat
		}
		first, err := translateResponseForTest(from, to, body, "fallback")
		if err != nil {
			t.Fatalf("%s first translation: %v", to, err)
		}
		second, err := translateResponseForTest(from, to, body, "fallback")
		if err != nil {
			t.Fatalf("%s second translation: %v", to, err)
		}
		a, err := parseCompletion(to, first)
		if err != nil {
			t.Fatal(err)
		}
		b, err := parseCompletion(to, second)
		if err != nil {
			t.Fatal(err)
		}
		if a.ID == "" || b.ID == "" || a.ID == b.ID {
			t.Fatalf("%s generated IDs = %q, %q", to, a.ID, b.ID)
		}
	}
}

func TestTranslateResponseGeminiMissingCallIDsAreResponseScoped(t *testing.T) {
	translate := func(responseID string) *completion {
		t.Helper()
		body := []byte(`{"responseId":"` + responseID + `","modelVersion":"m","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{}}}]},"finishReason":"STOP"}]}`)
		out, err := translateResponseForTest(Gemini, OpenAIChat, body, "fallback")
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseCompletion(OpenAIChat, out)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	a1 := translate("resp_a")
	a2 := translate("resp_a")
	b := translate("resp_b")
	want := completionGeneratedCallID("resp_a", 0)
	if a1.Blocks[0].Tool.CallID != want || a2.Blocks[0].Tool.CallID != want || b.Blocks[0].Tool.CallID == want {
		t.Fatalf("response-scoped IDs = %q, %q, %q; want first two %q", a1.Blocks[0].Tool.CallID, a2.Blocks[0].Tool.CallID, b.Blocks[0].Tool.CallID, want)
	}
}

func TestMarshalResponsesReassemblesMultipartMessage(t *testing.T) {
	body := []byte(`{"id":"resp","object":"response","model":"m","status":"completed","output":[{"id":"msg_provider","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"a","annotations":[]},{"type":"output_text","text":"b","annotations":[]}]}]}`)
	c, err := parseCompletion(OpenAIResponses, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureCompletionIdentities(c); err != nil {
		t.Fatal(err)
	}
	c.ResponsesEnvelope, err = openAIResponsesEnvelopeFromClient(responsesClientRequestForTest())
	if err != nil {
		t.Fatal(err)
	}
	out, err := marshalCompletion(OpenAIResponses, c)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Output []struct {
			ID      string            `json:"id"`
			Content []json.RawMessage `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Output) != 1 || wire.Output[0].ID != "msg_provider" || len(wire.Output[0].Content) != 2 {
		t.Fatalf("multipart message was split or duplicated: %s", out)
	}
}

func TestTranslateResponseRejectsInvalidDestinationToolIdentities(t *testing.T) {
	tests := []struct {
		name string
		from Protocol
		to   Protocol
		body string
	}{
		{"space in name", Gemini, OpenAIResponses, `{"responseId":"r","modelVersion":"m","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"c","name":"has space","args":{}}}]},"finishReason":"STOP"}]}`},
		{"control in opaque id", Gemini, OpenAIResponses, `{"responseId":"r","modelVersion":"m","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"bad\u0000id","name":"valid","args":{}}}]},"finishReason":"STOP"}]}`},
		{"anthropic id punctuation", Gemini, Anthropic, `{"responseId":"r","modelVersion":"m","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"bad.id/part","name":"valid","args":{}}}]},"finishReason":"STOP"}]}`},
		{"gemini leading digit", Anthropic, Gemini, `{"id":"r","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"call_1","name":"1invalid","input":{}}],"stop_reason":"tool_use"}`},
		{"portable max exceeded", Gemini, OpenAIResponses, `{"responseId":"r","modelVersion":"m","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"c","name":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","args":{}}}]},"finishReason":"STOP"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := translateResponseForTest(tc.from, tc.to, []byte(tc.body), "fallback")
			var unsupportedErr *UnsupportedError
			if !errors.As(err, &unsupportedErr) {
				t.Fatalf("invalid destination identity error = %v", err)
			}
		})
	}
}

func TestTranslatedOpenAIResponsesHasRequiredSDKEnvelope(t *testing.T) {
	client, err := ParseRequest(OpenAIResponses, []byte(`{
  "model":"gpt-client","input":"request","parallel_tool_calls":false,
  "tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
  "tool_choice":{"type":"function","name":"lookup"}
}`), requestPath(OpenAIResponses))
	if err != nil {
		t.Fatal(err)
	}
	source := []byte(`{"id":"msg_source","type":"message","role":"assistant","model":"claude-source","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":6,"cache_read_input_tokens":4,"cache_creation_input_tokens":0,"output_tokens":2}}`)
	out, err := TranslateResponseWithRequest(Anthropic, OpenAIResponses, source, "fallback", client)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"id", "created_at", "model", "object", "output", "parallel_tool_calls", "tool_choice", "tools"} {
		if _, ok := response[required]; !ok {
			t.Fatalf("missing SDK-required Response field %q: %s", required, out)
		}
	}
	for _, nullable := range []string{"error", "incomplete_details"} {
		if raw, ok := response[nullable]; !ok || string(raw) != "null" {
			t.Fatalf("Response field %q = %s, want explicit null", nullable, raw)
		}
	}
	var created int64
	if json.Unmarshal(response["created_at"], &created) != nil || created <= 0 {
		t.Fatalf("created_at = %s", response["created_at"])
	}
	var parallel bool
	if json.Unmarshal(response["parallel_tool_calls"], &parallel) != nil || parallel {
		t.Fatalf("parallel_tool_calls did not echo client request: %s", response["parallel_tool_calls"])
	}
	if !bytes.Contains(response["tool_choice"], []byte(`"name":"lookup"`)) || !bytes.Contains(response["tools"], []byte(`"name":"lookup"`)) {
		t.Fatalf("tool configuration did not echo client request: %s", out)
	}
	var usage struct {
		InputDetails struct {
			Cached int64 `json:"cached_tokens"`
			Write  int64 `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
		OutputDetails struct {
			Reasoning int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	}
	if json.Unmarshal(response["usage"], &usage) != nil || usage.InputDetails.Cached != 4 || usage.InputDetails.Write != 0 || usage.OutputDetails.Reasoning != 0 {
		t.Fatalf("SDK-required usage details missing: %s", response["usage"])
	}
}

func TestTranslatedChatHasRequiredCreatedField(t *testing.T) {
	source := []byte(`{"id":"msg_source","type":"message","role":"assistant","model":"claude-source","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	out, err := TranslateResponse(Anthropic, OpenAIChat, source, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(out, &response) != nil {
		t.Fatal("invalid translated Chat response")
	}
	var created int64
	if json.Unmarshal(response["created"], &created) != nil || created <= 0 {
		t.Fatalf("required Chat created field missing: %s", out)
	}
}

func TestResponseContextAndRequiredUsageFailures(t *testing.T) {
	anthropic := []byte(`{"id":"m","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	if _, err := TranslateResponse(Anthropic, OpenAIResponses, anthropic, "fallback"); err == nil {
		t.Fatal("cross-protocol Responses translation invented request echo fields without client context")
	}
	chatWithoutUsage := []byte(`{"id":"c","object":"chat.completion","created":1,"model":"gpt","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	if _, err := TranslateResponse(OpenAIChat, Anthropic, chatWithoutUsage, "fallback"); err == nil {
		t.Fatal("translated Anthropic response invented required usage")
	}
}

func TestCompleteClientResponseEnvelope(t *testing.T) {
	chatRaw := []byte(`{"id":"chat_host","object":"chat.completion","model":"m","choices":[]}`)
	chat, err := CompleteClientResponseEnvelope(OpenAIChat, chatRaw, nil)
	if err != nil {
		t.Fatal(err)
	}
	var chatObject map[string]json.RawMessage
	if json.Unmarshal(chat, &chatObject) != nil || len(chatObject["created"]) == 0 {
		t.Fatalf("host Chat envelope was not completed: %s", chat)
	}

	responsesRaw := []byte(`{"id":"resp_host","object":"response","model":"m","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`)
	responses, err := CompleteClientResponseEnvelope(OpenAIResponses, responsesRaw, responsesClientRequestForTest())
	if err != nil {
		t.Fatal(err)
	}
	var responseObject map[string]json.RawMessage
	if json.Unmarshal(responses, &responseObject) != nil {
		t.Fatalf("invalid completed Responses envelope: %s", responses)
	}
	for _, key := range []string{"created_at", "error", "incomplete_details", "parallel_tool_calls", "tool_choice", "tools"} {
		if _, ok := responseObject[key]; !ok {
			t.Fatalf("host Responses envelope missing %q: %s", key, responses)
		}
	}
	if !bytes.Contains(responseObject["usage"], []byte(`"cache_write_tokens":0`)) ||
		!bytes.Contains(responseObject["usage"], []byte(`"reasoning_tokens":0`)) {
		t.Fatalf("host Responses usage details missing: %s", responseObject["usage"])
	}
	if _, err := CompleteClientResponseEnvelope(OpenAIResponses, responsesRaw, nil); err == nil {
		t.Fatal("Responses envelope completed without original client context")
	}
	if _, err := CompleteClientResponseEnvelope(OpenAIChat, []byte(`{"id":"a","id":"b","object":"chat.completion","model":"m","choices":[]}`), nil); err == nil {
		t.Fatal("duplicate JSON member accepted")
	}
}

// TestExportSDKValidationFixtures is an opt-in fixture exporter for the pinned
// official-client contract check in scripts/validate-bridge-sdk.py. It remains
// skipped in ordinary Go test runs and performs no network access.
func TestExportSDKValidationFixtures(t *testing.T) {
	dir := os.Getenv("TORANA_BRIDGE_SDK_FIXTURE_DIR")
	if dir == "" {
		t.Skip("set TORANA_BRIDGE_SDK_FIXTURE_DIR to export SDK validation fixtures")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	anthropicSource := []byte(`{
  "id":"msg_sdk","type":"message","role":"assistant","model":"claude-sdk",
  "content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"call_sdk","name":"lookup","input":{"n":9007199254740993}}],
  "stop_reason":"tool_use","stop_sequence":null,
  "usage":{"input_tokens":3,"cache_read_input_tokens":1,"cache_creation_input_tokens":0,"output_tokens":2}
}`)
	chat, err := TranslateResponse(Anthropic, OpenAIChat, anthropicSource, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	client, err := ParseRequest(OpenAIResponses, []byte(`{
  "model":"gpt-client","input":"request","parallel_tool_calls":false,
  "tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
  "tool_choice":{"type":"function","name":"lookup"}
}`), requestPath(OpenAIResponses))
	if err != nil {
		t.Fatal(err)
	}
	responses, err := TranslateResponseWithRequest(Anthropic, OpenAIResponses, anthropicSource, "fallback", client)
	if err != nil {
		t.Fatal(err)
	}
	chatSource := []byte(`{
  "id":"chat_sdk","object":"chat.completion","created":1,"model":"gpt-sdk",
  "choices":[{"index":0,"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"call_sdk","type":"function","function":{"name":"lookup","arguments":"{\"n\":9007199254740993}"}}]},"finish_reason":"tool_calls"}],
  "usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6,"prompt_tokens_details":{"cached_tokens":1}}
}`)
	anthropic, err := TranslateResponse(OpenAIChat, Anthropic, chatSource, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	fixtures := map[string][]byte{
		"openai-chat.json":      chat,
		"openai-responses.json": responses,
		"anthropic.json":        anthropic,
	}
	for name, contents := range fixtures {
		if err := os.WriteFile(filepath.Join(dir, name), append(contents, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
